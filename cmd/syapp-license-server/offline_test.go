package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/license/signer"
)

// 正式签发全流程：在线侧生成请求 → 离线签名 → 在线侧核对导入 → 控制台可验证；吊销列表同理且是累积的。
func TestOfflineIssuanceFlow(t *testing.T) {
	dir := t.TempDir()
	// 这里验证请求 / 导入的格式与核对，私钥文件本身由 internal/license/signer 的测试覆盖，所以直接用内存中的密钥
	kf, pub, priv := testKey(t)
	keys := license.KeyRing{kf.KID: pub}
	act := filepath.Join(dir, "activation-request.json")
	if err := os.WriteFile(act, []byte(`{"installation_id":"inst_0123","fp_hashes":["a1","b2","c3","d4","e5"],"product":"EdgeTraffic OS"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reqPath := filepath.Join(dir, "sign-request-B0001.json")
	var out bytes.Buffer

	// 两张 License 进同一批次
	if err := cmdRequest([]string{"-batch", "B0001", "-kid", kf.KID, "-request", act, "-customer", "某某公司", "-edition", "enterprise",
		"-nodes", "50", "-id", "LIC-20261102-0001", "-out", reqPath}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdRequest([]string{"-kid", kf.KID, "-installation-id", "inst_9999", "-binding", "installation_only", "-customer", "另一家",
		"-edition", "standard", "-id", "LIC-20261102-0002", "-out", reqPath}, &out); err != nil {
		t.Fatal(err)
	}
	// 演示密钥不走这条流程；批次 / 密钥不一致不能追加
	if err := cmdRequest([]string{"-batch", "B0009", "-kid", "etos-demo-2026", "-installation-id", "inst_1", "-binding", "installation_only", "-customer", "x", "-out", filepath.Join(dir, "x.json")}, &out); err == nil {
		t.Fatal("demo kid accepted for a sign request")
	}
	if err := cmdRequest([]string{"-batch", "B0002", "-kid", kf.KID, "-installation-id", "inst_1", "-binding", "installation_only", "-customer", "x", "-out", reqPath}, &out); err == nil {
		t.Fatal("appended to a different batch")
	}
	raw, _ := os.ReadFile(reqPath)
	if bytes.Contains(raw, []byte("PRIVATE")) {
		t.Fatal("request contains key material")
	}
	req, err := license.ParseSignRequest(raw, time.Now())
	if err != nil || len(req.Licenses) != 2 || len(req.Licenses[0].Binding.FPHashes) != 5 {
		t.Fatalf("request: %+v %v", req, err)
	}

	// 离线签名机
	signed, err := signer.SignBatch(req, raw, kf.KID, priv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signedPath := filepath.Join(dir, "signed-B0001.json")
	sb, _ := json.Marshal(signed)
	if err := os.WriteFile(signedPath, sb, 0o644); err != nil {
		t.Fatal(err)
	}

	// 在线侧导入
	licDir := filepath.Join(dir, "licenses")
	if err := cmdImport([]string{"-request", reqPath, "-signed", signedPath, "-out", licDir}, &out, keys); err != nil {
		t.Fatal(err)
	}
	lb, err := os.ReadFile(filepath.Join(licDir, "LIC-20261102-0001.lic"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := license.Verify(string(lb), keys)
	if err != nil || p.Customer.Name != "某某公司" || p.Edition != license.EditionEnterprise || !p.HasFeature(license.FeatureGMTLS) {
		t.Fatalf("issued license: %+v %v", p, err)
	}
	// 被改动的签名结果、未内置的密钥：导入失败
	tampered := bytes.Replace(sb, []byte("LIC-20261102-0002\",\"raw\":\"ey"), []byte("LIC-20261102-0002\",\"raw\":\"ez"), 1)
	if err := os.WriteFile(signedPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdImport([]string{"-request", reqPath, "-signed", signedPath, "-out", licDir}, &out, keys); err == nil {
		t.Fatal("tampered signed batch imported")
	}
	if err := os.WriteFile(signedPath, sb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdImport([]string{"-request", reqPath, "-signed", signedPath, "-out", licDir}, &out, license.KeyRing{}); err == nil {
		t.Fatal("imported without a trusted public key")
	}

	// 吊销列表：第一份
	crlReq := filepath.Join(dir, "sign-request-B0002.json")
	if err := cmdCRLRequest([]string{"-batch", "B0002", "-kid", kf.KID, "-revoke", "LIC-20261102-0002", "-reason", "退款", "-out", crlReq}, &out, keys); err != nil {
		t.Fatal(err)
	}
	craw, _ := os.ReadFile(crlReq)
	creq, err := license.ParseSignRequest(craw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cs, err := signer.SignBatch(creq, craw, kf.KID, priv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c1, err := license.VerifyCRL(cs.CRL, keys)
	if err != nil || c1.Serial != 1 || !c1.LicenseRevoked("LIC-20261102-0002") {
		t.Fatalf("crl 1: %+v %v", c1, err)
	}
	if !strings.Contains(out.String(), "序号 1") {
		t.Fatalf("output: %s", out.String())
	}
}

// 第二份吊销列表以第一份为基础：序号加 1，保留之前的吊销项。
func TestCRLRequestIsCumulative(t *testing.T) {
	dir := t.TempDir()
	kf, pub, priv := testKey(t)
	keys := license.KeyRing{kf.KID: pub}
	prev, _ := license.SignCRL(&license.CRL{V: 1, KID: kf.KID, Serial: 7, RevokedLicenses: []license.RevokedLicense{{LicenseID: "LIC-OLD-1"}}}, priv)
	base := filepath.Join(dir, "crl-7.crl")
	if err := os.WriteFile(base, []byte(prev), 0o644); err != nil {
		t.Fatal(err)
	}
	reqPath := filepath.Join(dir, "sign-request-B0003.json")
	var out bytes.Buffer
	if err := cmdCRLRequest([]string{"-batch", "B0003", "-kid", kf.KID, "-base", base, "-revoke", "LIC-NEW-2", "-out", reqPath}, &out, keys); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(reqPath)
	req, err := license.ParseSignRequest(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if req.CRL.Serial != 8 || !req.CRL.LicenseRevoked("LIC-OLD-1") || !req.CRL.LicenseRevoked("LIC-NEW-2") {
		t.Fatalf("not cumulative: %+v", req.CRL)
	}
	// 伪造的 -base（未受信任的密钥签的）不能作为基础
	_, other, _ := testKey(t)
	if err := cmdCRLRequest([]string{"-batch", "B0004", "-kid", kf.KID, "-base", base, "-out", filepath.Join(dir, "r4.json")}, &out, license.KeyRing{kf.KID: other}); err == nil {
		t.Fatal("untrusted base CRL accepted")
	}
}

type testKF struct{ KID string }

func testKey(t *testing.T) (testKF, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKF{KID: "etos-prod-2027"}, pub, priv
}
