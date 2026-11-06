package conn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/selfupdate"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

// 数据面热升级的 agent 端（规格书 12.6）：下载升级包与签名到投放目录 → wrapper upgrade-start（以 root 验签、切换、
// USR2 / WINCH）→ 探活；探活失败立即 upgrade-abort 回到旧版本。COMMIT / ABORT 由控制台在确认后下发。

var upgradeVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$`)

const maxPackage = 128 << 20

func (a *Agent) upgradeDir() string { return a.path("upgrade") }

func (a *Agent) wrapperRun(ctx context.Context, args ...string) (string, error) {
	w := a.C.Apply.Wrapper
	out, err := exec.CommandContext(ctx, w[0], append(append([]string(nil), w[1:]...), args...)...).CombinedOutput()
	return string(out), err
}

func fetchPackage(ctx context.Context, client agentv1.AgentGatewayClient, req *agentv1.FetchPackageRequest) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := client.FetchPackage(cctx, req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		c, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if int64(buf.Len()) != c.Offset {
			return nil, errors.New("out-of-order chunk")
		}
		buf.Write(c.Data)
		if buf.Len() > maxPackage {
			return nil, errors.New("package too large")
		}
		if c.Last {
			break
		}
	}
	return buf.Bytes(), nil
}

func (a *Agent) handleUpgrade(ctx context.Context, client agentv1.AgentGatewayClient, u *agentv1.UpgradeDataplane,
	reply func(agentv1.CommandResult_Status, string, string), send func(*agentv1.AgentMessage)) {
	switch u.Action {
	case agentv1.UpgradeDataplane_COMMIT:
		out, err := a.wrapperRun(ctx, "upgrade-commit")
		if err != nil {
			reply(agentv1.CommandResult_INVALID, "upgrade-commit failed", out)
			return
		}
		reply(agentv1.CommandResult_APPLIED, "upgrade committed", out)
		// 重新上报版本信息，控制台节点页随即显示新版本
		send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Hello{Hello: a.hello(ctx)}})
		return
	case agentv1.UpgradeDataplane_ABORT:
		out, err := a.wrapperRun(ctx, "upgrade-abort")
		if err != nil {
			reply(agentv1.CommandResult_DEGRADED, "upgrade-abort failed", out)
			return
		}
		reply(agentv1.CommandResult_ROLLED_BACK, "upgrade aborted", out)
		return
	case agentv1.UpgradeDataplane_START:
	default:
		reply(agentv1.CommandResult_REJECTED, "unknown upgrade action", "")
		return
	}
	if !upgradeVersionRe.MatchString(u.Version) {
		reply(agentv1.CommandResult_REJECTED, "invalid version", "")
		return
	}
	if ok, why := a.licenseWritable(); !ok {
		reply(agentv1.CommandResult_REJECTED, "license does not allow upgrades: "+why, "")
		return
	}
	sig, err := pkgsig.Parse(u.SignatureJson)
	if err != nil || sig.Name != brand.ID() || sig.Version != u.Version || sig.Arch != runtime.GOARCH || sig.SHA256 != u.PackageSha256 {
		reply(agentv1.CommandResult_REJECTED, "signature file does not match this upgrade", "")
		return
	}
	data, err := fetchPackage(ctx, client, &agentv1.FetchPackageRequest{Name: brand.ID(), Version: u.Version, Arch: runtime.GOARCH, Variant: sig.Variant})
	if err != nil {
		reply(agentv1.CommandResult_REJECTED, "fetch package: "+err.Error(), "")
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sig.SHA256 {
		reply(agentv1.CommandResult_REJECTED, "package sha256 mismatch", "")
		return
	}
	// 投放目录：agent 可写；wrapper 以 root 读入内存后验签（agent 自身不需要信任这些字节）
	if err := os.MkdirAll(a.upgradeDir(), 0o755); err != nil {
		reply(agentv1.CommandResult_REJECTED, err.Error(), "")
		return
	}
	base := filepath.Join(a.upgradeDir(), brand.ID()+"-"+u.Version+".tar.gz")
	if err := os.WriteFile(base+".part", data, 0o644); err != nil || os.Rename(base+".part", base) != nil ||
		os.WriteFile(base+".sig", u.SignatureJson, 0o644) != nil {
		reply(agentv1.CommandResult_REJECTED, "write package failed", "")
		return
	}
	// 节点装有 syedge-lua 时，控制台同时下发目标版本附加包的签名：一起投放，由 wrapper 验签后与二进制一起切换
	if len(u.LuaSignatureJson) > 0 {
		ls, err := pkgsig.Parse(u.LuaSignatureJson)
		if err != nil || ls.Name != brand.Lua() || ls.Version != u.Version || ls.Arch != runtime.GOARCH || ls.Variant != "lua" {
			reply(agentv1.CommandResult_REJECTED, "add-on signature does not match this upgrade", "")
			return
		}
		ld, err := fetchPackage(ctx, client, &agentv1.FetchPackageRequest{Name: brand.Lua(), Version: u.Version, Arch: runtime.GOARCH, Variant: "lua"})
		if err != nil {
			reply(agentv1.CommandResult_REJECTED, "fetch add-on package: "+err.Error(), "")
			return
		}
		if sum := sha256.Sum256(ld); hex.EncodeToString(sum[:]) != ls.SHA256 {
			reply(agentv1.CommandResult_REJECTED, "add-on package sha256 mismatch", "")
			return
		}
		lb := filepath.Join(a.upgradeDir(), brand.Lua()+"-"+u.Version+".tar.gz")
		if err := os.WriteFile(lb+".part", ld, 0o644); err != nil || os.Rename(lb+".part", lb) != nil ||
			os.WriteFile(lb+".sig", u.LuaSignatureJson, 0o644) != nil {
			reply(agentv1.CommandResult_REJECTED, "write add-on package failed", "")
			return
		}
	}
	a.log.Info("starting data-plane hot upgrade", "version", u.Version)
	out, err := a.wrapperRun(ctx, "upgrade-start", u.Version)
	if err != nil {
		reply(agentv1.CommandResult_REJECTED, "upgrade-start failed (the running version is unchanged)", out)
		return
	}
	time.Sleep(time.Second) // 新 worker 就绪
	if err := a.applier.PostChecks(ctx, checks(u.PostChecks)); err != nil {
		aout, aerr := a.wrapperRun(ctx, "upgrade-abort")
		if aerr != nil {
			reply(agentv1.CommandResult_DEGRADED, "post check failed and upgrade-abort also failed: "+err.Error(), aout)
			return
		}
		reply(agentv1.CommandResult_ROLLED_BACK, "post check failed, back on the previous version: "+err.Error(), out)
		return
	}
	reply(agentv1.CommandResult_APPLIED, "new version is serving; waiting for commit", out)
}

// handleAgentUpgrade 下载并校验 agent 升级包，安装后退出，由 systemd 重启到新版本（syedge 不受影响）。
func (a *Agent) handleAgentUpgrade(ctx context.Context, client agentv1.AgentGatewayClient, u *agentv1.UpgradeAgent, reply func(agentv1.CommandResult_Status, string, string)) {
	if !upgradeVersionRe.MatchString(u.Version) {
		reply(agentv1.CommandResult_REJECTED, "invalid version", "")
		return
	}
	sig, err := pkgsig.Parse(u.SignatureJson)
	if err != nil || sig.Name != brand.Agent() || sig.Version != u.Version || sig.Arch != runtime.GOARCH {
		reply(agentv1.CommandResult_REJECTED, "signature file does not match this upgrade", "")
		return
	}
	data, err := fetchPackage(ctx, client, &agentv1.FetchPackageRequest{Name: brand.Agent(), Version: u.Version, Arch: runtime.GOARCH, Variant: sig.Variant})
	if err != nil {
		reply(agentv1.CommandResult_REJECTED, "fetch package: "+err.Error(), "")
		return
	}
	if _, err := selfupdate.Install(a.C.StateDir, data, u.SignatureJson, runtime.GOARCH, pkgsig.Keys()); err != nil {
		reply(agentv1.CommandResult_REJECTED, err.Error(), "")
		return
	}
	reply(agentv1.CommandResult_APPLIED, "agent "+u.Version+" installed; restarting the agent process", "")
	a.log.Info("agent self-upgrade installed, exiting for restart", "version", u.Version)
	go func() {
		time.Sleep(2 * time.Second)
		a.exit(0)
	}()
}
