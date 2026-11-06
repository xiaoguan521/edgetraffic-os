package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/license/signer"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

// 离线签名工具不得链接任何网络代码。
func TestNoNetworkCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "net", dep == "crypto/tls", dep == "os/exec",
			strings.HasPrefix(dep, "net/"), strings.HasPrefix(dep, "golang.org/x/net"), strings.HasPrefix(dep, "google.golang.org/grpc"):
			t.Errorf("offline signer depends on %s", dep)
		}
	}
}

type harness struct {
	t   *testing.T
	dir string
	out bytes.Buffer
	err bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, dir: t.TempDir()}
}

func (h *harness) run(stdin string, args ...string) int {
	h.out.Reset()
	h.err.Reset()
	a := &app{in: bufio.NewReader(strings.NewReader(stdin)), out: &h.out, errw: &h.err,
		now: func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }, sysNet: filepath.Join(h.dir, "nonet")}
	a.prompt = func(string) ([]byte, error) { h.t.Fatal("unexpected interactive prompt"); return nil, nil }
	return a.run(args)
}

func (h *harness) path(name string) string { return filepath.Join(h.dir, name) }

func writeRequest(t *testing.T, path, kid, batch string) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	p := license.Payload{V: 1, KID: kid, LicenseID: "LIC-20261102-0001", Customer: license.Customer{Name: "某某公司"},
		Edition: license.EditionStandard, Features: []license.Feature{license.FeatureCore}, Limits: license.Limits{Nodes: 10, Sites: 100},
		IssuedAt: now, NotBefore: now, ExpiresAt: now.AddDate(1, 0, 0), MaintenanceUntil: now.AddDate(1, 0, 0), GraceDays: 15,
		Binding: license.Binding{Type: "installation_only", InstallationID: "inst_abc"}}
	r := license.SignRequest{Format: license.SignRequestFormat, Batch: batch, KID: kid, CreatedAt: now, CreatedBy: "ops", Licenses: []license.Payload{p}}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCLIFlow(t *testing.T) {
	h := newHarness(t)
	pf := h.path("pass")
	if err := os.WriteFile(pf, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := h.path("usb")

	if rc := h.run("", "keygen", "-kid", "etos-prod-2027", "-out", keys, "-passphrase-file", pf); rc != 0 {
		t.Fatalf("keygen: %s", h.err.String())
	}
	keyPath := filepath.Join(keys, "etos-prod-2027.key.json")
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if !strings.Contains(h.out.String(), `"etos-prod-2027": "`) || !strings.Contains(h.out.String(), "公钥指纹") {
		t.Fatalf("keygen output: %s", h.out.String())
	}
	kb, _ := os.ReadFile(keyPath)
	if strings.Contains(string(kb), "PRIVATE KEY") {
		t.Fatal("plaintext PEM private key written")
	}
	// 不覆盖已有密钥
	if rc := h.run("", "keygen", "-kid", "etos-prod-2027", "-out", keys, "-passphrase-file", pf); rc == 0 {
		t.Fatal("existing key overwritten")
	}

	req := h.path("sign-request-B0012.json")
	writeRequest(t, req, "etos-prod-2027", "B0012")
	if rc := h.run("", "inspect", "-in", req); rc != 0 || !strings.Contains(h.out.String(), "某某公司") {
		t.Fatalf("inspect: %d %s %s", rc, h.out.String(), h.err.String())
	}
	log := h.path("signer-log.jsonl")
	// 没有输入批次号确认：取消
	if rc := h.run("B0011\n", "sign", "-key", keyPath, "-in", req, "-log", log, "-passphrase-file", pf); rc == 0 || !strings.Contains(h.err.String(), "取消") {
		t.Fatalf("sign without confirmation: %d %s", rc, h.err.String())
	}
	if _, err := os.Stat(h.path("signed-B0012.json")); err == nil {
		t.Fatal("signed file written without confirmation")
	}
	// 输入批次号确认后签名
	if rc := h.run("B0012\n", "sign", "-key", keyPath, "-in", req, "-log", log, "-passphrase-file", pf); rc != 0 {
		t.Fatalf("sign: %s", h.err.String())
	}
	if rc := h.run("", "verify", "-key", keyPath, "-request", req, "-signed", h.path("signed-B0012.json")); rc != 0 {
		t.Fatalf("verify: %s", h.err.String())
	}
	// 同一份请求不会签第二次
	if rc := h.run("", "sign", "-key", keyPath, "-in", req, "-log", log, "-passphrase-file", pf, "-yes", "-out", h.path("again.json")); rc == 0 || !strings.Contains(h.err.String(), "已经签过") {
		t.Fatalf("duplicate signing: %d %s", rc, h.err.String())
	}
	// 口令错误
	bad := h.path("bad")
	if err := os.WriteFile(bad, []byte("this is not the passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	req2 := h.path("sign-request-B0013.json")
	writeRequest(t, req2, "etos-prod-2027", "B0013")
	if rc := h.run("", "sign", "-key", keyPath, "-in", req2, "-log", log, "-passphrase-file", bad, "-yes"); rc == 0 || !strings.Contains(h.err.String(), "口令错误") {
		t.Fatalf("wrong passphrase: %d %s", rc, h.err.String())
	}
	// 批次要求的密钥与私钥文件不符
	req3 := h.path("sign-request-B0014.json")
	writeRequest(t, req3, "etos-prod-2028", "B0014")
	if rc := h.run("", "sign", "-key", keyPath, "-in", req3, "-log", log, "-passphrase-file", pf, "-yes"); rc == 0 {
		t.Fatal("signed a batch for another key")
	}
	if rc := h.run("", "pubkey", "-key", keyPath); rc != 0 || !strings.Contains(h.out.String(), "fingerprint:") {
		t.Fatalf("pubkey: %s", h.err.String())
	}
}

func TestOfflineCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	h := newHarness(t)
	sys := h.path("nonet")
	if err := os.MkdirAll(filepath.Join(sys, "lo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "lo", "operstate"), []byte("unknown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "eth0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "eth0", "operstate"), []byte("down\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{errw: &h.err, sysNet: sys}
	if err := a.offlineCheck(true); err != nil {
		t.Fatalf("down interface reported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sys, "eth0", "operstate"), []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.offlineCheck(true); err == nil || !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("connected interface not detected: %v", err)
	}
	if err := a.offlineCheck(false); err != nil || !strings.Contains(h.err.String(), "警告") {
		t.Fatalf("warning mode: %v %s", err, h.err.String())
	}
}

// 升级包签名（规格书 12.6）：只接受 etos-pkg-* 密钥，签名文件能被 pkgsig 校验；License 密钥不能签升级包。
func TestSignPackage(t *testing.T) {
	h := newHarness(t)
	pf := h.path("pass.txt")
	if err := os.WriteFile(pf, []byte("correct horse battery staple"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := h.path("keys")
	if rc := h.run("", "keygen", "-kid", "etos-pkg-2027", "-out", keys, "-passphrase-file", pf); rc != 0 {
		t.Fatalf("keygen pkg: %s", h.err.String())
	}
	if rc := h.run("", "keygen", "-kid", "etos-prod-2027", "-out", keys, "-passphrase-file", pf); rc != 0 {
		t.Fatalf("keygen prod: %s", h.err.String())
	}
	pkg := h.path("syedge-1.30.7-1-amd64.tar.gz")
	_ = os.WriteFile(pkg, []byte("fake package"), 0o644)
	if rc := h.run("", "sign-package", "-key", filepath.Join(keys, "etos-prod-2027.key.json"), "-in", pkg, "-version", "1.30.7-1", "-arch", "amd64", "-passphrase-file", pf, "-yes"); rc == 0 {
		t.Fatal("license key accepted for packages")
	}
	if rc := h.run("1.30.7-0\n", "sign-package", "-key", filepath.Join(keys, "etos-pkg-2027.key.json"), "-in", pkg, "-version", "1.30.7-1", "-arch", "amd64", "-passphrase-file", pf); rc == 0 {
		t.Fatal("wrong confirmation accepted")
	}
	if rc := h.run("1.30.7-1\n", "sign-package", "-key", filepath.Join(keys, "etos-pkg-2027.key.json"), "-in", pkg, "-version", "1.30.7-1", "-arch", "amd64", "-passphrase-file", pf); rc != 0 {
		t.Fatalf("sign-package: %s", h.err.String())
	}
	sb, err := os.ReadFile(pkg + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := pkgsig.Parse(sb)
	if err != nil {
		t.Fatal(err)
	}
	kfb, _ := os.ReadFile(filepath.Join(keys, "etos-pkg-2027.key.json"))
	kf, _ := signer.ParseKeyFile(kfb)
	pub, _ := kf.Public()
	if err := pkgsig.Verify(sig, []byte("fake package"), map[string]ed25519.PublicKey{"etos-pkg-2027": pub}); err != nil {
		t.Fatal(err)
	}
}
