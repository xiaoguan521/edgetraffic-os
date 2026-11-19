package conn

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/cmdsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// approvedEnv 是一个带假 wrapper 的 agent：发布两个版本 r000001、r000002（当前为 r000002）。
type approvedEnv struct {
	t        *testing.T
	a        *Agent
	confRoot string
	exec     func(*agentv1.ServerMessage) *agentv1.CommandResult
	d1, d2   string
	wrapper  string
	bundles  map[string][]byte
	files1   map[string][]byte
}

func newApprovedEnv(t *testing.T) *approvedEnv {
	t.Helper()
	dir := t.TempDir()
	confRoot := filepath.Join(dir, "conf")
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(filepath.Join(confRoot, "releases"), 0o755)
	_ = os.MkdirAll(stateDir, 0o700)
	pidFile, errLog := filepath.Join(dir, "syedge.pid"), filepath.Join(dir, "error.log")
	_ = os.WriteFile(pidFile, []byte("1"), 0o644)
	_ = os.WriteFile(errLog, nil, 0o644)
	wrapper := filepath.Join(dir, "wrapper.sh")
	// 存在 wrapper.sh.failreload 文件时 reload 失败（模拟配置校验通过但进程没有接受）
	_ = os.WriteFile(wrapper, []byte("#!/bin/sh\nif [ \"$1\" = reload ] && [ -e \"$0.failreload\" ]; then exit 1; fi\nexit 0\n"), 0o755)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	licPub, licPriv, _ := ed25519.GenerateKey(rand.Reader)
	gen := 100
	a := New(Config{StateDir: stateDir, Apply: apply.Config{
		ConfRoot: confRoot, PidFile: pidFile, ErrorLog: errLog, LockFile: filepath.Join(dir, "apply.lock"),
		Wrapper: []string{wrapper}, ConfirmWait: 20 * time.Millisecond, Workers: func(int) []int { gen++; return []int{gen} },
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.verifier = &cmdsig.Verifier{Pub: pub}
	a.licKeys = license.KeyRing{license.CIKeyID: licPub}
	bundles := map[string][]byte{}
	cl := &bundleClient{bundles: bundles}
	seq := 0
	e := &approvedEnv{t: t, a: a, confRoot: confRoot, wrapper: wrapper, bundles: bundles}
	e.exec = func(m *agentv1.ServerMessage) *agentv1.CommandResult {
		seq++
		m.CmdId = time.Now().Format("150405.000000000") + "-" + string(rune('0'+seq))
		if err := cmdsig.Sign(priv, m, time.Now()); err != nil {
			t.Fatal(err)
		}
		var res *agentv1.CommandResult
		a.handle(context.Background(), cl, m, func(am *agentv1.AgentMessage) {
			if r, ok := am.Body.(*agentv1.AgentMessage_Result); ok {
				res = r.Result
			}
		})
		return res
	}
	lic := signTestLicense(t, licPriv, time.Now().Add(24*time.Hour))
	if r := e.exec(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_License{License: &agentv1.LicenseUpdate{Raw: lic}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("license: %+v", r)
	}
	c, k := testCertPair(t, "shop.example.com")
	b1, d1, f1 := renderSiteBundle(t, "r000001", c, k, "")
	b2, d2, _ := renderSiteBundle(t, "r000002", c, k, "/v2")
	bundles["r000001"], bundles["r000002"] = b1, b2
	for _, s := range []struct{ rev, parent, dg string }{{"r000001", "", d1}, {"r000002", "r000001", d2}} {
		if r := e.exec(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: s.rev, ParentRevisionId: s.parent, BundleSha256: s.dg}}}); r.Status != agentv1.CommandResult_APPLIED {
			t.Fatalf("apply %s: %+v", s.rev, r)
		}
	}
	e.d1, e.d2, e.files1 = d1, d2, f1
	return e
}

func (e *approvedEnv) rollback(rev string) *agentv1.CommandResult {
	return e.exec(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Rollback{Rollback: &agentv1.Rollback{ToRevisionId: rev}}})
}

// forgeSelfConsistent 把版本目录整体换成“文件与清单彼此一致、但摘要不同于已批准摘要”的内容。
func (e *approvedEnv) forgeSelfConsistent(rev string) {
	e.t.Helper()
	files := map[string][]byte{}
	for k, v := range e.files1 {
		files[k] = v
	}
	files["conf.d/site-shop.conf"] = []byte("# injected by local writer\n")
	b := render.Repack(rev, render.RebaseRevision(files, "r000001", rev))
	dir := filepath.Join(e.confRoot, "releases", rev)
	if err := os.RemoveAll(dir); err != nil {
		e.t.Fatal(err)
	}
	if err := b.WriteDir(dir); err != nil {
		e.t.Fatal(err)
	}
}

// 本地历史版本被改动后，回滚不能复用该目录。
func TestRollbackRefusesModifiedHistoricRevision(t *testing.T) {
	e := newApprovedEnv(t)
	_ = os.WriteFile(filepath.Join(e.confRoot, "releases", "r000001", "conf.d", "site-shop.conf"), []byte("# tampered\n"), 0o644)
	if r := e.rollback("r000001"); r.Status == agentv1.CommandResult_APPLIED {
		t.Fatalf("rollback reused a modified revision: %+v", r)
	}
	if cur := e.a.applier.Current(); cur != "r000002" {
		t.Fatalf("current switched to %s", cur)
	}
}

// 文件与 MANIFEST 一并改成自洽的内容时，只有对照已批准摘要才能发现。
func TestRollbackRefusesSelfConsistentForgery(t *testing.T) {
	e := newApprovedEnv(t)
	e.forgeSelfConsistent("r000001")
	if r := e.rollback("r000001"); r.Status == agentv1.CommandResult_APPLIED {
		t.Fatalf("rollback accepted a forged but self-consistent revision: %+v", r)
	}
	if cur := e.a.applier.Current(); cur != "r000002" {
		t.Fatalf("current switched to %s", cur)
	}
}

// 未被改动的历史版本仍然可以回滚（含授权过期时：只用本地数据核验，不依赖控制台）。
func TestRollbackStillWorksForIntactRevision(t *testing.T) {
	e := newApprovedEnv(t)
	if r := e.rollback("r000001"); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("intact rollback failed: %+v", r)
	}
	if cur := e.a.applier.Current(); cur != "r000001" {
		t.Fatalf("current = %s", cur)
	}
}

// 重连时不得用本地目录的摘要覆盖已批准摘要。
func TestReconnectKeepsApprovedDigest(t *testing.T) {
	e := newApprovedEnv(t)
	e.forgeSelfConsistent("r000002")
	before := e.a.approvedDigest("r000002")
	if before == "" {
		t.Fatal("no approved digest recorded after apply")
	}
	e.a.recordCurrentApproved()
	if after := e.a.approvedDigest("r000002"); after != before {
		t.Fatalf("approved digest overwritten on reconnect: %s -> %s", before, after)
	}
}

// 升级自没有 approved.json 的旧版 agent：首次连接时记录当前（自洽的）摘要，以便之后的恢复可用。
func TestReconnectRecordsDigestWhenNoneRecorded(t *testing.T) {
	e := newApprovedEnv(t)
	_ = os.Remove(e.a.path("approved.json"))
	e.a.recordCurrentApproved()
	if e.a.approvedDigest("r000002") == "" {
		t.Fatal("first-seen digest was not recorded")
	}
}

// approved.json 损坏不同于“没有记录”：不得退回旧节点的只核对自洽性，回滚必须拒绝。
func TestRollbackRefusesWhenApprovedRecordIsCorrupt(t *testing.T) {
	e := newApprovedEnv(t)
	e.forgeSelfConsistent("r000001")
	if err := os.WriteFile(e.a.path("approved.json"), []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.rollback("r000001")
	if r.Status == agentv1.CommandResult_APPLIED {
		t.Fatalf("rollback accepted a forged revision with a corrupt approval record: %+v", r)
	}
	if !strings.Contains(r.Message, "approved.json") {
		t.Fatalf("message should name the corrupt record: %q", r.Message)
	}
}

// 记录损坏时新的批准记录不得把整个文件从空表重写（会丢掉全部历史摘要）；文件原样保留供人工处理。
func TestRecordApprovedKeepsCorruptFile(t *testing.T) {
	e := newApprovedEnv(t)
	bad := []byte("{malformed")
	if err := os.WriteFile(e.a.path("approved.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.recordApproved("r000009", "sha256:x")
	got, _ := os.ReadFile(e.a.path("approved.json"))
	if !bytes.Equal(got, bad) {
		t.Fatalf("corrupt approval record was rewritten: %q", got)
	}
}

// 没有记录文件（旧节点）仍按既有兼容路径回滚，不受影响。
func TestRollbackWithoutApprovedFileStillWorks(t *testing.T) {
	e := newApprovedEnv(t)
	_ = os.Remove(e.a.path("approved.json"))
	if r := e.rollback("r000001"); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("legacy rollback failed: %+v", r)
	}
}

// 漂移恢复在 reload 阶段失败时，磁盘上的当前目录必须与失败前一致（手工改动过的原目录换回来），
// 不能留下“干净副本已就位、进程却没接受”的状态；保留备份目录也不应遗留。
func TestRestoreFailureSwapsTheOriginalDirectoryBack(t *testing.T) {
	e := newApprovedEnv(t)
	site := filepath.Join(e.confRoot, "releases", "r000002", "conf.d", "site-shop.conf")
	if err := os.WriteFile(site, []byte("# hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.wrapper+".failreload", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.exec(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000002", ParentRevisionId: "r000002", BundleSha256: e.d2}}})
	if r.Status == agentv1.CommandResult_APPLIED {
		t.Fatalf("restore reported success although reload failed: %+v", r)
	}
	if got, _ := os.ReadFile(site); string(got) != "# hand edited\n" {
		t.Fatalf("current directory was left as the clean copy after a failed restore: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(e.confRoot, "releases", ".drift-*")); len(left) != 0 {
		t.Fatalf("backup directory left behind: %v", left)
	}
	if left, _ := filepath.Glob(filepath.Join(e.confRoot, "releases", "*.restore")); len(left) != 0 {
		t.Fatalf("staging directory left behind: %v", left)
	}
}

// 授权只读时的漂移恢复：approved.json 损坏不能被当成“没有记录”而退回信任当前目录清单。
func TestReadOnlyRestoreRefusesWhenApprovedRecordIsCorrupt(t *testing.T) {
	e := newApprovedEnv(t)
	rev := "r000002"
	e.forgeSelfConsistent(rev)
	files := map[string][]byte{}
	for name, data := range e.files1 {
		files[name] = data
	}
	files["conf.d/site-shop.conf"] = []byte("# injected by local writer\n")
	forged := render.Repack(rev, render.RebaseRevision(files, "r000001", rev))
	bundle, err := forged.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	if forged.Manifest.Digest == e.d2 {
		t.Fatal("precondition: forged and approved digests are equal")
	}
	if err := os.WriteFile(e.a.path("approved.json"), []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.licRaw = "invalid-test-license"
	e.a.mu.Unlock()
	if ok, _ := e.a.licenseWritable(); ok {
		t.Fatal("precondition: license must be read-only")
	}
	e.bundles[rev] = bundle
	r := e.exec(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{
		RevisionId: rev, ParentRevisionId: rev, BundleSha256: forged.Manifest.Digest,
	}}})
	if r.Status == agentv1.CommandResult_APPLIED {
		t.Fatalf("read-only restore accepted a forged revision with a corrupt approval record: %+v", r)
	}
}
