// Package conn 实现 syapp-agent 与控制台之间的连接：首次注册、mTLS 长连接、心跳与指令执行（规格书 5.2、5.3）。
//
// agent 只主动连接控制台，不监听任何端口；每条指令先验应用层签名再执行。
package conn

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/collect"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/cmdsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

// Config 是 agent 的运行参数。
type Config struct {
	Server       string         // 控制台 gRPC 地址 host:port
	StateDir     string         // /var/lib/syapp-agent
	DataplaneBin string         // /opt/syapp/sbin/syapp（只读取 -V 与 sha256，不直接执行变更）
	StatusURL    string         // http://127.0.0.1:8081/__syapp_status
	VersionURL   string         // http://127.0.0.1:8081/__syapp_version：本实例 worker 实际运行的版本
	Profile      render.Profile // 本机实例档案（默认实例为零值），注册与上线时上报
	AccessLog    string         // /var/log/syapp/access.json
	Heartbeat    time.Duration
	Apply        apply.Config
	Modules      string // /opt/syapp/modules（syapp-lua 附加包，只读取版本信息上报）
}

// state 是注册后持久化的身份信息。
type state struct {
	NodeID     string `json:"node_id"`
	Server     string `json:"server"`
	CommandPub []byte `json:"command_pub"`
}

// Agent 是一个边缘节点探针实例。
type Agent struct {
	C       Config
	applier *apply.Applier
	log     *slog.Logger

	mu       sync.Mutex
	st       state
	verifier *cmdsig.Verifier
	licRaw   string
	licKeys  license.KeyRing // 为空时使用 license.BuiltinKeyRing()

	lastDrift string
	exit      func(code int) // 自升级后退出（测试可替换），默认 os.Exit // 最近一次上报的漂移文件状态指纹（heartbeat goroutine 独占）
}

// New 创建 agent。
func New(c Config, log *slog.Logger) *Agent {
	if c.Heartbeat == 0 {
		c.Heartbeat = 15 * time.Second
	}
	return &Agent{C: c, applier: &apply.Applier{C: c.Apply}, log: log, exit: os.Exit}
}

func (a *Agent) path(n string) string { return filepath.Join(a.C.StateDir, n) }

// Enrolled 报告本机是否已完成注册。
func (a *Agent) Enrolled() bool {
	_, err := os.Stat(a.path("node.json"))
	return err == nil
}

// Enroll 用一次性令牌完成注册。caFingerprint 是控制台 CA 证书 DER 的 sha256，
// 由“添加节点”页面生成的安装命令提供，用于首次连接时固定控制台身份。
func (a *Agent) Enroll(ctx context.Context, token, caFingerprint string) error {
	if err := os.MkdirAll(a.C.StateDir, 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	_ = pub
	host, _ := os.Hostname()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}, priv)
	if err != nil {
		return err
	}
	var seenCA []byte
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // 由 VerifyPeerCertificate 固定 CA 指纹并校验证书链
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			for _, der := range raw {
				if fp(der) == strings.ToLower(caFingerprint) {
					ca, err := x509.ParseCertificate(der)
					if err != nil {
						return err
					}
					leaf, err := x509.ParseCertificate(raw[0])
					if err != nil {
						return err
					}
					pool := x509.NewCertPool()
					pool.AddCert(ca)
					if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
						return err
					}
					seenCA = der
					return nil
				}
			}
			return errors.New("console CA fingerprint mismatch")
		},
	}
	cc, err := grpc.NewClient(a.C.Server, grpc.WithNoProxy(), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := agentv1.NewAgentGatewayClient(cc).Enroll(cctx, &agentv1.EnrollRequest{
		EnrollToken: token, CsrDer: csr, Hostname: host,
		MachineFingerprint: machineFingerprint(), AgentVersion: version.Version,
		Instance: instanceInfo(a.C.Profile), Brand: brand.ID(),
	})
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	if !bytes.Equal(resp.CaCertDer, seenCA) {
		return errors.New("enroll: CA in response differs from pinned CA")
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(priv)
	files := map[string][]byte{
		"node.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}),
		"node.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.ClientCertDer}),
		"ca.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.CaCertDer}),
	}
	// 每个文件先写临时文件再 rename；node.json 最后写，它的出现代表注册完成
	for n, b := range files {
		if err := a.writeAtomic(n, b); err != nil {
			return err
		}
	}
	st, _ := json.MarshalIndent(state{NodeID: resp.NodeId, Server: a.C.Server, CommandPub: resp.CommandPubkey}, "", "  ")
	if err := a.writeAtomic("node.json", st); err != nil {
		return err
	}
	a.log.Info("enrolled", "node_id", resp.NodeId)
	return nil
}

// writeAtomic 写入身份文件。enroll 通常以 root 执行，而 agent 以 syapp-agent 运行：
// 文件属主跟随状态目录，运行中的 agent 才能读取新身份。
func (a *Agent) writeAtomic(name string, b []byte) error {
	tmp := a.path(name + ".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if fi, err := os.Stat(a.C.StateDir); err == nil && os.Geteuid() == 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(tmp, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, a.path(name))
}

func fp(der []byte) string { s := sha256.Sum256(der); return hex.EncodeToString(s[:]) }

func machineFingerprint() string {
	b, _ := os.ReadFile("/etc/machine-id")
	return fp(bytes.TrimSpace(b))
}

func (a *Agent) load() (*tls.Config, error) {
	b, err := os.ReadFile(a.path("node.json"))
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(a.path("node.crt"), a.path("node.key"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(a.path("ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("bad ca.crt")
	}
	a.mu.Lock()
	a.st = st
	a.verifier = &cmdsig.Verifier{Pub: ed25519.PublicKey(st.CommandPub)}
	a.mu.Unlock()
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}

// Run 保持与控制台的连接，断线后指数退避重连（1s → 60s），直到 ctx 结束。
// 每次重连前重新读取注册身份：节点被吊销后在本机重新注册，运行中的 agent 无需重启即可换用新身份。
func (a *Agent) Run(ctx context.Context) error {
	tlsCfg, err := a.load()
	if err != nil {
		return fmt.Errorf("not enrolled: %w", err)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		ident := a.identityStamp()
		err := a.session(ctx, tlsCfg)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if grpcstatus.Code(err) == codes.PermissionDenied {
			a.log.Error("console rejected this node (revoked?); re-add it in the console and run the enroll command on this host; "+brand.ID()+" keeps serving", "err", err)
		} else {
			a.log.Warn("disconnected from console; "+brand.ID()+" keeps serving", "err", err, "retry_in", backoff)
		}
		// 等待退避时间，期间若本机重新注册则立即重连
		deadline := time.After(backoff)
	wait:
		for {
			select {
			case <-ctx.Done():
				break wait
			case <-deadline:
				break wait
			case <-time.After(2 * time.Second):
				if a.identityStamp() != ident {
					backoff = time.Second / 2
					break wait
				}
			}
		}
		if cfg, err := a.load(); err == nil {
			tlsCfg = cfg
		} else {
			a.log.Warn("reload identity failed; keeping current one", "err", err)
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
	return ctx.Err()
}

// identityStamp 标识当前注册身份（node.json 的修改时间与大小），用于发现本机重新注册。
func (a *Agent) identityStamp() string {
	fi, err := os.Stat(a.path("node.json"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
}

func (a *Agent) session(ctx context.Context, tlsCfg *tls.Config) error {
	cc, err := grpc.NewClient(a.C.Server, grpc.WithNoProxy(), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	client := agentv1.NewAgentGatewayClient(cc)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Connect(sctx)
	if err != nil {
		return err
	}
	out := make(chan *agentv1.AgentMessage, 16)
	errc := make(chan error, 2)
	go func() { // 唯一的发送者
		for {
			select {
			case <-sctx.Done():
				return
			case m := <-out:
				if err := stream.Send(m); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	send := func(m *agentv1.AgentMessage) {
		m.MsgId = newID()
		select {
		case out <- m:
		case <-sctx.Done():
		}
	}
	send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Hello{Hello: a.hello(sctx)}})
	if cur := a.applier.Current(); cur != "" {
		if d, err := a.applier.TreeDigest(); err == nil && !strings.HasPrefix(d, "drifted") {
			a.recordApproved(cur, d)
		}
	}
	a.log.Info("connected to console", "server", a.C.Server)

	go a.heartbeat(sctx, send)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			go a.handle(sctx, client, m, send)
		}
	}()
	return <-errc
}

func (a *Agent) hello(ctx context.Context) *agentv1.Hello {
	h := &agentv1.Hello{AgentVersion: version.Version + " linux/" + runtime.GOARCH, AppliedRevisionId: a.applier.Current(), MachineFingerprint: machineFingerprint(),
		Instance: instanceInfo(a.C.Profile), Brand: brand.ID(), RunningVersion: a.runningVersion(ctx)}
	if out, err := exec.CommandContext(ctx, a.C.DataplaneBin, "-V").CombinedOutput(); err == nil {
		h.SyappVersionOutput = string(out)
	}
	if b, err := os.ReadFile(a.C.DataplaneBin); err == nil {
		h.SyappSha256 = fp(b)
	}
	h.LuaAddon = a.luaAddon()
	return h
}

// runningVersion 返回本实例 worker 实际运行的数据面版本；配置中还没有版本路径（旧版本）时为空。
func (a *Agent) runningVersion(ctx context.Context) string {
	if a.C.VersionURL == "" {
		return ""
	}
	v, err := collect.ReadVersion(ctx, a.C.VersionURL)
	if err != nil {
		return ""
	}
	return v
}

var luaVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$`)

// luaAddon 返回已安装的 syapp-lua 附加包：<包版本>/<基础变体>；未安装或不完整时为空。
func (a *Agent) luaAddon() string {
	dir := a.C.Modules
	if dir == "" {
		dir = layout.System("").Modules()
	}
	vb, err := os.ReadFile(filepath.Join(dir, "lua", "VERSION"))
	if err != nil {
		return ""
	}
	bb, _ := os.ReadFile(filepath.Join(dir, "lua", "BASE"))
	var v struct {
		Version string `json:"version"`
	}
	base := strings.TrimSpace(string(bb))
	if json.Unmarshal(vb, &v) != nil || !luaVersionRe.MatchString(v.Version) || (base != "standard" && base != "gmssl") {
		return ""
	}
	return v.Version + "/" + base
}

func (a *Agent) heartbeat(ctx context.Context, send func(*agentv1.AgentMessage)) {
	host := &collect.Host{}
	acc := &collect.AccessLog{Path: a.C.AccessLog}
	acc.SkipToEnd()
	host.Sample()
	t := time.NewTicker(a.C.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cpu, mem := host.Sample()
			hb := &agentv1.Heartbeat{TsUnix: now.Unix(), CpuPercent: cpu, MemUsedBytes: mem, LuaAddon: a.luaAddon(), RunningVersion: a.runningVersion(ctx)}
			if st, err := collect.ReadStub(ctx, a.C.StatusURL); err == nil {
				hb.ActiveConnections, hb.RequestsTotal = st.Active, st.Requests
			}
			if d, err := a.applier.TreeDigest(); err == nil {
				hb.TreeHash = d
				if strings.HasPrefix(d, "drifted") {
					a.reportDrift(send)
				} else {
					a.lastDrift = ""
				}
			}
			for _, m := range acc.Collect(now) {
				hb.SiteMinutes = append(hb.SiteMinutes, &agentv1.SiteMinute{
					Site: m.Site, MinuteUnix: m.Minute, Requests: m.Requests,
					Status_2Xx: m.S2xx, Status_3Xx: m.S3xx, Status_4Xx: m.S4xx, Status_5Xx: m.S5xx,
					P50Ms: m.P50ms, P95Ms: m.P95ms, BytesSent: m.Bytes,
				})
			}
			send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Heartbeat{Heartbeat: hb}})
		}
	}
}

// reportDrift 在发现漂移时上报受管版本目录内的差异（同一文件状态只上报一次，规格书 12.7）。
func (a *Agent) reportDrift(send func(*agentv1.AgentMessage)) {
	r, err := a.applier.Drift()
	if err != nil || r == nil || r.TreeHash == a.lastDrift {
		return
	}
	a.lastDrift = r.TreeHash
	rep := &agentv1.DriftReport{RevisionId: r.Revision, TreeHash: r.TreeHash}
	for _, f := range r.Files {
		ch := agentv1.DriftFile_MODIFIED
		switch f.Change {
		case "added":
			ch = agentv1.DriftFile_ADDED
		case "removed":
			ch = agentv1.DriftFile_REMOVED
		}
		rep.Files = append(rep.Files, &agentv1.DriftFile{Path: f.Path, Change: ch, Sha256: f.SHA256, Size: f.Size, Content: f.Content, ContentOmitted: f.Omitted})
	}
	send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Drift{Drift: rep}})
}

func (a *Agent) handle(ctx context.Context, client agentv1.AgentGatewayClient, m *agentv1.ServerMessage, send func(*agentv1.AgentMessage)) {
	reply := func(st agentv1.CommandResult_Status, msg, stderr string) {
		send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Result{Result: &agentv1.CommandResult{CmdId: m.CmdId, Status: st, Message: msg, Stderr: stderr}}})
	}
	a.mu.Lock()
	v := a.verifier
	a.mu.Unlock()
	if err := v.Verify(m, time.Now()); err != nil {
		a.log.Warn("rejected unsigned or replayed command", "cmd_id", m.CmdId, "err", err)
		reply(agentv1.CommandResult_REJECTED, "signature check failed: "+err.Error(), "")
		return
	}
	switch b := m.Body.(type) {
	case *agentv1.ServerMessage_License:
		a.mu.Lock()
		a.licRaw = b.License.Raw
		a.mu.Unlock()
		ok, why := a.licenseWritable()
		a.log.Info("license updated", "writable", ok, "reason", why)
		reply(agentv1.CommandResult_APPLIED, "license received", "")
	case *agentv1.ServerMessage_Apply:
		bundle, err := fetch(ctx, client, b.Apply.RevisionId)
		if err != nil {
			reply(agentv1.CommandResult_REJECTED, "fetch bundle: "+err.Error(), "")
			return
		}
		restore := b.Apply.RevisionId == b.Apply.ParentRevisionId
		if ok, why := a.licenseWritable(); !ok {
			// 授权只读时只放行两类操作，均由 agent 自己核对，不信任控制台的任何标记（规格书 8.6）：
			// 证书续期（只有证书文件变化），以及漂移恢复（内容与线上版本已批准的清单完全一致）。
			switch {
			case restore && a.sameAsApproved(b.Apply.RevisionId, bundle):
				a.log.Info("license is read-only; allowing drift restore", "revision", b.Apply.RevisionId)
			case !restore && a.certOnly(bundle):
				a.log.Info("license is read-only; allowing certificate-only release", "revision", b.Apply.RevisionId)
			default:
				reply(agentv1.CommandResult_REJECTED, "license does not allow new releases: "+why, "")
				return
			}
		}
		if restore {
			a.log.Info("restoring drifted release", "revision", b.Apply.RevisionId)
			r := a.applier.Restore(ctx, b.Apply.RevisionId, bundle, b.Apply.BundleSha256, checks(b.Apply.PostChecks))
			if r.Status == apply.Applied {
				a.recordApproved(b.Apply.RevisionId, b.Apply.BundleSha256)
			}
			a.log.Info("restore result", "revision", b.Apply.RevisionId, "status", r.Status, "message", r.Message)
			reply(status(r.Status), r.Message, r.Stderr)
			return
		}
		a.log.Info("applying", "revision", b.Apply.RevisionId, "parent", b.Apply.ParentRevisionId)
		r := a.applier.Apply(ctx, b.Apply.RevisionId, b.Apply.ParentRevisionId, bundle, b.Apply.BundleSha256, checks(b.Apply.PostChecks))
		if r.Status == apply.Applied {
			a.recordApproved(b.Apply.RevisionId, b.Apply.BundleSha256)
		}
		a.log.Info("apply result", "revision", b.Apply.RevisionId, "status", r.Status, "message", r.Message)
		reply(status(r.Status), r.Message, r.Stderr)
	case *agentv1.ServerMessage_Rollback:
		r := a.applier.Rollback(ctx, b.Rollback.ToRevisionId, nil)
		if r.Status == apply.Applied {
			if d := a.approvedDigest(b.Rollback.ToRevisionId); d != "" {
				a.recordApproved(b.Rollback.ToRevisionId, d)
			}
		}
		a.log.Info("rollback result", "revision", b.Rollback.ToRevisionId, "status", r.Status, "message", r.Message)
		reply(status(r.Status), r.Message, r.Stderr)
	case *agentv1.ServerMessage_Probe:
		out, err := exec.CommandContext(ctx, a.C.Apply.Wrapper[0], append(append([]string(nil), a.C.Apply.Wrapper[1:]...), "test", b.Probe.RevisionId)...).CombinedOutput()
		if err != nil {
			reply(agentv1.CommandResult_INVALID, "configuration test failed", string(out))
			return
		}
		reply(agentv1.CommandResult_APPLIED, "configuration test passed", string(out))
	case *agentv1.ServerMessage_Signal:
		var sig string
		switch b.Signal.Kind {
		case agentv1.Signal_REOPEN:
			sig = "reopen"
		default:
			reply(agentv1.CommandResult_REJECTED, "signal not allowed", "")
			return
		}
		out, err := exec.CommandContext(ctx, a.C.Apply.Wrapper[0], append(append([]string(nil), a.C.Apply.Wrapper[1:]...), sig)...).CombinedOutput()
		if err != nil {
			reply(agentv1.CommandResult_INVALID, sig+" failed", string(out))
			return
		}
		reply(agentv1.CommandResult_APPLIED, sig+" done", "")
	case *agentv1.ServerMessage_Upgrade:
		a.handleUpgrade(ctx, client, b.Upgrade, reply, send)
	case *agentv1.ServerMessage_UpgradeAgent:
		a.handleAgentUpgrade(ctx, client, b.UpgradeAgent, reply)
	default:
		reply(agentv1.CommandResult_REJECTED, "command not supported by this agent", "")
	}
}

// certOnly 判断新版本相对线上版本是否只替换了证书文件。
func (a *Agent) certOnly(bundle []byte) bool {
	next, err := render.ReadTarGz(bundle)
	if err != nil {
		return false
	}
	cur, err := a.applier.CurrentFiles()
	if err != nil {
		return false
	}
	return render.CertOnlyChange(cur, next)
}

// sameAsApproved 判断包的清单摘要是否与当前版本已批准的清单摘要一致（即恢复为已批准的内容）。
// 优先使用 StateDir/approved.json 中记录的不可变摘要（防版本目录内 MANIFEST.json 被手工改删后无法恢复），
// 若尚未记录则回退到当前版本目录中的 MANIFEST.json。
func (a *Agent) sameAsApproved(rev string, bundle []byte) bool {
	next, err := render.ReadTarGz(bundle)
	if err != nil {
		return false
	}
	var nm struct {
		Revision string `json:"revision"`
		Digest   string `json:"digest"`
	}
	if json.Unmarshal(next[render.ManifestName], &nm) != nil || nm.Digest == "" || (rev != "" && nm.Revision != rev) {
		return false
	}
	if render.Repack(nm.Revision, next).Manifest.Digest != nm.Digest {
		return false
	}
	if want := a.approvedDigest(rev); want != "" {
		return nm.Digest == want
	}
	cur, err := a.applier.CurrentFiles()
	if err != nil {
		return false
	}
	var cm struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal(cur[render.ManifestName], &cm) != nil {
		return false
	}
	return cm.Digest != "" && nm.Digest == cm.Digest
}

// recordApproved 在 StateDir/approved.json 中记录已成功生效的 (revision -> manifest digest)。
func (a *Agent) recordApproved(rev, digest string) {
	if a.C.StateDir == "" || rev == "" || digest == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	m := map[string]string{}
	if b, err := os.ReadFile(a.path("approved.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[rev] = digest
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(a.path("approved.json"), b, 0o600)
	}
}

func (a *Agent) approvedDigest(rev string) string {
	if a.C.StateDir == "" || rev == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var m map[string]string
	if b, err := os.ReadFile(a.path("approved.json")); err == nil && json.Unmarshal(b, &m) == nil {
		return m[rev]
	}
	return ""
}

// licenseWritable：尚未收到 License 时由控制台负责拦截；收到后按 agent 侧独立校验结果执行。
func (a *Agent) licenseWritable() (bool, string) {
	a.mu.Lock()
	raw := a.licRaw
	kr := a.licKeys
	a.mu.Unlock()
	if raw == "" {
		return true, ""
	}
	if kr == nil {
		kr = license.BuiltinKeyRing()
	}
	return license.AgentCheck(raw, kr, time.Now())
}

func fetch(ctx context.Context, client agentv1.AgentGatewayClient, rev string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, err := client.FetchBundle(cctx, &agentv1.FetchBundleRequest{RevisionId: rev})
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
		if buf.Len() > 64<<20 {
			return nil, errors.New("bundle too large")
		}
		if c.Last {
			break
		}
	}
	return buf.Bytes(), nil
}

func checks(in []*agentv1.PostCheck) []apply.PostCheck {
	var out []apply.PostCheck
	for _, c := range in {
		pc := apply.PostCheck{Host: c.Host, Path: c.Path, HTTPS: c.Https}
		for _, s := range c.ExpectStatus {
			pc.ExpectStatus = append(pc.ExpectStatus, int(s))
		}
		out = append(out, pc)
	}
	return out
}

func status(s apply.Status) agentv1.CommandResult_Status {
	switch s {
	case apply.Applied:
		return agentv1.CommandResult_APPLIED
	case apply.Invalid:
		return agentv1.CommandResult_INVALID
	case apply.Conflict:
		return agentv1.CommandResult_CONFLICT
	case apply.RolledBack:
		return agentv1.CommandResult_ROLLED_BACK
	case apply.Degraded:
		return agentv1.CommandResult_DEGRADED
	default:
		return agentv1.CommandResult_REJECTED
	}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// instanceInfo 把本机实例档案转换为上报格式（只是数据，不是指令）。
func instanceInfo(p render.Profile) *agentv1.InstanceInfo {
	e := p.Effective()
	return &agentv1.InstanceInfo{Name: e.Instance, Mode: e.Mode, Root: e.Root,
		HttpPort: uint32(e.HTTPPort), HttpsPort: uint32(e.HTTPSPort), StatusPort: uint32(e.StatusPort)}
}
