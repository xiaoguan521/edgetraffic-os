package conn

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/cmdsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

type bundleClient struct {
	agentv1.AgentGatewayClient
	bundles map[string][]byte
}

func (b *bundleClient) FetchBundle(_ context.Context, in *agentv1.FetchBundleRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.BundleChunk], error) {
	data, ok := b.bundles[in.RevisionId]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &fakeStream{chunks: []*agentv1.BundleChunk{{Data: data, Offset: 0, Last: true}}}, nil
}

func signTestLicense(t *testing.T, priv ed25519.PrivateKey, expiresAt time.Time) string {
	t.Helper()
	now := time.Now().UTC()
	raw, err := license.Sign(&license.Payload{
		V:         1,
		KID:       license.CIKeyID,
		LicenseID: "LIC-EXP-TEST",
		Customer:  license.Customer{Name: "test"},
		Edition:   license.EditionEnterprise,
		Features:  []license.Feature{license.FeatureCore, license.FeatureGMTLS},
		Limits:    license.Limits{Nodes: 10, Sites: 50},
		IssuedAt:  now.Add(-30 * 24 * time.Hour),
		NotBefore: now.Add(-30 * 24 * time.Hour),
		ExpiresAt: expiresAt,
		Binding:   license.Binding{Type: "installation_only", InstallationID: "inst_test"},
	}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testCertPair(t *testing.T, host string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}

func renderSiteBundle(t *testing.T, rev, certPEM, keyPEM, extraRoutePath string) ([]byte, string, map[string][]byte) {
	t.Helper()
	routes := []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}
	if extraRoutePath != "" {
		routes = append(routes, render.Route{Path: extraRoutePath, Match: "prefix", Mode: "deny"})
	}
	in := render.Input{
		Revision:  rev,
		MimeTypes: render.DefaultMimeTypes(),
		Sites: []render.Site{{
			Name:    "shop",
			Domains: []string{"shop.example.com"},
			HTTPS:   &render.TLS{CertPEM: certPEM, KeyPEM: keyPEM},
			Routes:  routes,
		}},
	}
	b, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	tgz, err := b.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	return tgz, b.Manifest.Digest, b.Files
}

// 验证过期授权下的应急操作闭环（CERT-05 / DRF-03 / 规格书 8.6）：
// 1. 有效授权下发布 r000001 与 r000002；
// 2. 下发过期授权后，普通配置变更（r000003）被 Agent REJECTED；
// 3. 过期授权下仅替换证书的新版本（r000004）被 Agent 放行并 APPLIED；
// 4. 节点配置被手工篡改，甚至 MANIFEST.json 被删除/破坏后，过期授权下的漂移恢复（Restore r000004）仍凭 StateDir/approved.json 放行并 APPLIED；
// 5. 过期授权下回滚到历史版本（Rollback r000001）放行并 APPLIED。
func TestExpiredLicenseEmergencyOperations(t *testing.T) {
	dir := t.TempDir()
	confRoot := filepath.Join(dir, "conf")
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(filepath.Join(confRoot, "releases"), 0o755)
	_ = os.MkdirAll(stateDir, 0o700)

	pidFile := filepath.Join(dir, "syedge.pid")
	errLog := filepath.Join(dir, "error.log")
	_ = os.WriteFile(pidFile, []byte("1"), 0o644)
	_ = os.WriteFile(errLog, nil, 0o644)
	wrapper := filepath.Join(dir, "wrapper.sh")
	_ = os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	licPub, licPriv, _ := ed25519.GenerateKey(rand.Reader)
	gen := 100
	a := New(Config{
		StateDir: stateDir,
		Apply: apply.Config{
			ConfRoot:    confRoot,
			PidFile:     pidFile,
			ErrorLog:    errLog,
			LockFile:    filepath.Join(dir, "apply.lock"),
			Wrapper:     []string{wrapper},
			ConfirmWait: 20 * time.Millisecond,
			Workers:     func(int) []int { gen++; return []int{gen} },
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.verifier = &cmdsig.Verifier{Pub: pub}
	a.licKeys = license.KeyRing{license.CIKeyID: licPub}

	bundles := map[string][]byte{}
	cl := &bundleClient{bundles: bundles}
	seq := 0
	execCmd := func(m *agentv1.ServerMessage) *agentv1.CommandResult {
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

	// 1. 有效授权下发布 r000001 和 r000002
	validLic := signTestLicense(t, licPriv, time.Now().Add(24*time.Hour))
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_License{License: &agentv1.LicenseUpdate{Raw: validLic}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("push valid license: %+v", r)
	}
	c1, k1 := testCertPair(t, "shop.example.com")
	b1, d1, _ := renderSiteBundle(t, "r000001", c1, k1, "")
	bundles["r000001"] = b1
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000001", ParentRevisionId: "", BundleSha256: d1}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("apply r000001: %+v", r)
	}
	b2, d2, f2 := renderSiteBundle(t, "r000002", c1, k1, "/v2")
	bundles["r000002"] = b2
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000002", ParentRevisionId: "r000001", BundleSha256: d2}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("apply r000002: %+v", r)
	}

	// 2. 切换为过期授权
	expiredLic := signTestLicense(t, licPriv, time.Now().Add(-48*time.Hour))
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_License{License: &agentv1.LicenseUpdate{Raw: expiredLic}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("push expired license: %+v", r)
	}

	// 普通新发布 r000003 必须被拒绝
	b3, d3, _ := renderSiteBundle(t, "r000003", c1, k1, "/v3")
	bundles["r000003"] = b3
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000003", ParentRevisionId: "r000002", BundleSha256: d3}}}); r.Status != agentv1.CommandResult_REJECTED || !strings.Contains(r.Message, "license expired") {
		t.Fatalf("ordinary release under expired license: %+v", r)
	}

	// 3. 证书续期版本 r000004（仅替换证书与私钥文件）在过期授权下必须放行
	c2, k2 := testCertPair(t, "shop.example.com")
	renewFiles := render.RebaseRevision(f2, "r000002", "r000004")
	renewFiles["certs/site-shop.crt"] = []byte(c2)
	renewFiles["certs/site-shop.key"] = []byte(k2)
	rb4 := render.Repack("r000004", renewFiles)
	tgz4, err := rb4.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	bundles["r000004"] = tgz4
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000004", ParentRevisionId: "r000002", BundleSha256: rb4.Manifest.Digest}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("CERT-05: cert renewal under expired license failed: %+v", r)
	}
	if a.applier.Current() != "r000004" {
		t.Fatalf("current = %s, want r000004", a.applier.Current())
	}

	// 4. 漂移恢复（DRF-03）：篡改站点配置并故意删除 MANIFEST.json，验证漂移可检测且过期授权下可恢复
	r4Dir := filepath.Join(confRoot, "releases", "r000004")
	_ = os.WriteFile(filepath.Join(r4Dir, "conf.d", "site-shop.conf"), []byte("# tampered\n"), 0o644)
	_ = os.Remove(filepath.Join(r4Dir, render.ManifestName))
	if td, err := a.applier.TreeDigest(); err != nil || !strings.HasPrefix(td, "drifted:") {
		t.Fatalf("drift with missing manifest not detected: %q %v", td, err)
	}
	if dr, err := a.applier.Drift(); err != nil || dr == nil || len(dr.Files) == 0 {
		t.Fatalf("Drift() with missing manifest: %+v %v", dr, err)
	}
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000004", ParentRevisionId: "r000004", BundleSha256: rb4.Manifest.Digest}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("DRF-03: drift restore under expired license failed: %+v", r)
	}
	if td, err := a.applier.TreeDigest(); err != nil || td != rb4.Manifest.Digest {
		t.Fatalf("tree digest after restore = %q (%v), want %q", td, err, rb4.Manifest.Digest)
	}

	// 伪造的“恢复包”（夹带未批准变更但谎称是 r000004 恢复）在过期授权下必须被拒绝
	fakeFiles := render.RebaseRevision(f2, "r000002", "r000004")
	fakeFiles["conf.d/site-shop.conf"] = []byte("# injected\n")
	fakeBundle := render.Repack("r000004", fakeFiles)
	fakeTgz, _ := fakeBundle.TarGz()
	bundles["r000004"] = fakeTgz
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000004", ParentRevisionId: "r000004", BundleSha256: fakeBundle.Manifest.Digest}}}); r.Status != agentv1.CommandResult_REJECTED {
		t.Fatalf("forged restore bundle accepted under expired license: %+v", r)
	}

	// 5. 过期授权下回滚到 r000001
	if r := execCmd(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Rollback{Rollback: &agentv1.Rollback{ToRevisionId: "r000001"}}}); r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("rollback under expired license failed: %+v", r)
	}
	if a.applier.Current() != "r000001" {
		t.Fatalf("current after rollback = %s, want r000001", a.applier.Current())
	}
}
