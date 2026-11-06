package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
)

var now = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// 本包的大多数测试关注格式与流程，用轻量的口令派生参数加快运行（-race 下 256 MiB 很慢）；
// 生产参数由 TestProductionKDF 单独覆盖。
func TestMain(m *testing.M) {
	kdfParams = KDF{Alg: "argon2id", Time: 1, MemoryKiB: 8 * 1024, Threads: 1}
	minMemoryKiB = 8 * 1024
	os.Exit(m.Run())
}

// 用生产参数完整走一遍：生成的文件记录的就是生产参数，并且能用口令打开。
func TestProductionKDF(t *testing.T) {
	savedParams, savedMin := kdfParams, minMemoryKiB
	kdfParams, minMemoryKiB = ProductionKDF, 64*1024
	defer func() { kdfParams, minMemoryKiB = savedParams, savedMin }()
	kf, pub, err := Generate("etos-prod-2027", []byte(pass), now)
	if err != nil {
		t.Fatal(err)
	}
	if kf.KDF.Time != 3 || kf.KDF.MemoryKiB != 256*1024 || kf.KDF.Threads != 4 || kf.KDF.Alg != "argon2id" {
		t.Fatalf("key file not written with production parameters: %+v", kf.KDF)
	}
	priv, err := kf.Open([]byte(pass))
	if err != nil || !pub.Equal(priv.Public()) {
		t.Fatalf("open with production parameters: %v", err)
	}
	// 生产下限：低于 64 MiB 的文件拒绝打开（防止被替换成弱参数的文件）
	weak := *kf
	weak.KDF.MemoryKiB = 8 * 1024
	if _, err := weak.Open([]byte(pass)); err == nil {
		t.Fatal("weak KDF parameters accepted under the production minimum")
	}
}

const pass = "correct horse battery staple"

func TestKeyFileRoundTrip(t *testing.T) {
	kf, pub, err := Generate("etos-prod-2027", []byte(pass), now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(kf)
	kf2, err := ParseKeyFile(b)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := kf2.Open([]byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(priv.Public()) {
		t.Fatal("public key mismatch")
	}
	seed := priv.Seed()
	for _, enc := range []string{base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed), hex.EncodeToString(seed)} {
		if bytes.Contains(b, []byte(enc)) {
			t.Fatal("plaintext private key found in the key file")
		}
	}
	msg := []byte("hello")
	if !ed25519.Verify(pub, msg, ed25519.Sign(priv, msg)) {
		t.Fatal("signature")
	}
	Wipe(priv)
	for _, c := range priv {
		if c != 0 {
			t.Fatal("wipe failed")
		}
	}
}

// 私钥文件里不能出现明文私钥；口令错误、文件被改动（kid / 公钥 / 密文）都无法解密。
func TestKeyFileTamperAndWrongPass(t *testing.T) {
	kf, _, err := Generate("etos-prod-2027", []byte(pass), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kf.Open([]byte("wrong passphrase 1234")); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	for name, mut := range map[string]func(k *KeyFile){
		"kid": func(k *KeyFile) { k.KID = "etos-prod-2028" },
		"pub": func(k *KeyFile) {
			o, _, _ := Generate("etos-prod-2027", []byte(pass), now)
			k.PublicKey = o.PublicKey
		},
		"data": func(k *KeyFile) { c := []byte(k.Ciphertext); c[5] ^= 1; k.Ciphertext = string(c) },
	} {
		k := *kf
		mut(&k)
		if _, err := k.Open([]byte(pass)); err == nil {
			t.Errorf("%s tampering not detected", name)
		}
	}
}

func TestGenerateRules(t *testing.T) {
	if _, _, err := Generate("etos-prod-2027", []byte("short"), now); err == nil {
		t.Fatal("short passphrase accepted")
	}
	for _, kid := range []string{"etos-demo-2027", "etos-ci-ephemeral", "prod-2027"} {
		if _, _, err := Generate(kid, []byte(pass), now); err == nil {
			t.Errorf("kid %s accepted", kid)
		}
	}
}

func payload(id, kid string) license.Payload {
	return license.Payload{V: 1, KID: kid, LicenseID: id, Customer: license.Customer{ID: "C1", Name: "某某公司"},
		Edition: license.EditionProfessional, Features: []license.Feature{license.FeatureCore},
		Limits:   license.Limits{Nodes: 50, Sites: 2000, AuditDays: 365},
		IssuedAt: now, NotBefore: now, ExpiresAt: now.AddDate(1, 0, 0), MaintenanceUntil: now.AddDate(1, 0, 0), GraceDays: 15,
		Binding: license.Binding{Type: "installation_only", InstallationID: "inst_abc"},
		Product: license.Product{Name: "SyEdge", MinVersion: "0.0.0", MaxVersion: "1.*"}}
}

func request(t *testing.T, kid string) ([]byte, *license.SignRequest) {
	r := &license.SignRequest{Format: license.SignRequestFormat, Batch: "B0012", KID: kid, CreatedAt: now, CreatedBy: "ops",
		Licenses: []license.Payload{payload("LIC-20261102-0001", kid), payload("LIC-20261102-0002", kid)},
		CRL: &license.CRL{V: 1, KID: kid, Serial: 4, IssuedAt: now,
			RevokedLicenses: []license.RevokedLicense{{LicenseID: "LIC-20250101-0009", RevokedAt: now, Reason: "退款"}}}}
	raw, _ := json.MarshalIndent(r, "", "  ")
	parsed, err := license.ParseSignRequest(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	return raw, parsed
}

// 完整流程：生成密钥 → 后台导出请求 → 离线签名 → 后台导入核对。
func TestSignAndVerifyBatch(t *testing.T) {
	kf, pub, _ := Generate("etos-prod-2027", []byte(pass), now)
	priv, err := kf.Open([]byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	defer Wipe(priv)
	raw, req := request(t, kf.KID)
	out, err := SignBatch(req, raw, kf.KID, priv, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := license.KeyRing{kf.KID: pub}
	if err := license.VerifySignedBatch(req, raw, out, keys); err != nil {
		t.Fatal(err)
	}
	// 签出的 License 能被控制台验签
	p, err := license.Verify(out.Licenses[0].Raw, keys)
	if err != nil || p.LicenseID != "LIC-20261102-0001" {
		t.Fatalf("%v %v", p, err)
	}
	if c, err := license.VerifyCRL(out.CRL, keys); err != nil || c.Serial != 4 {
		t.Fatalf("crl %v %v", c, err)
	}
	// 签名结果被调换、对应另一份请求、少一条、被改动：都拒绝
	other := append([]byte(nil), raw...)
	other = bytes.Replace(other, []byte(`"ops"`), []byte(`"eve"`), 1)
	if err := license.VerifySignedBatch(req, other, out, keys); err == nil {
		t.Fatal("signed batch accepted for a different request")
	}
	short := *out
	short.Licenses = out.Licenses[:1]
	if err := license.VerifySignedBatch(req, raw, &short, keys); err == nil {
		t.Fatal("missing entry accepted")
	}
	swapped := *out
	swapped.Licenses = []license.SignedLicense{{LicenseID: out.Licenses[0].LicenseID, Raw: out.Licenses[1].Raw}, out.Licenses[1]}
	if err := license.VerifySignedBatch(req, raw, &swapped, keys); err == nil {
		t.Fatal("swapped entries accepted")
	}
	// 用另一把密钥签的（例如有人自己生成一把同名密钥）：拒绝
	kf2, _, _ := Generate("etos-prod-2027", []byte(pass), now)
	priv2, _ := kf2.Open([]byte(pass))
	forged, err := SignBatch(req, raw, kf2.KID, priv2, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := license.VerifySignedBatch(req, raw, forged, keys); err == nil {
		t.Fatal("batch signed by another key accepted")
	}
}

func TestSignBatchKIDMismatch(t *testing.T) {
	kf, _, _ := Generate("etos-prod-2028", []byte(pass), now)
	priv, _ := kf.Open([]byte(pass))
	raw, req := request(t, "etos-prod-2027")
	if _, err := SignBatch(req, raw, kf.KID, priv, now); err == nil {
		t.Fatal("signed with the wrong key")
	}
}

func TestRequestValidation(t *testing.T) {
	_, base := request(t, "etos-prod-2027")
	cases := map[string]func(r *license.SignRequest){
		"format":     func(r *license.SignRequest) { r.Format = "x" },
		"batch":      func(r *license.SignRequest) { r.Batch = "12; rm" },
		"empty":      func(r *license.SignRequest) { r.Licenses, r.CRL = nil, nil },
		"kid mix":    func(r *license.SignRequest) { r.Licenses[0].KID = "etos-prod-2026" },
		"dup id":     func(r *license.SignRequest) { r.Licenses[1].LicenseID = r.Licenses[0].LicenseID },
		"bad id":     func(r *license.SignRequest) { r.Licenses[0].LicenseID = "../x" },
		"no inst":    func(r *license.SignRequest) { r.Licenses[0].Binding.InstallationID = "" },
		"fp binding": func(r *license.SignRequest) { r.Licenses[0].Binding.Type = "installation" },
		"dates":      func(r *license.SignRequest) { r.Licenses[0].ExpiresAt = r.Licenses[0].NotBefore },
		"edition":    func(r *license.SignRequest) { r.Licenses[0].Edition = "gold" },
		"crl self":   func(r *license.SignRequest) { r.CRL.RevokedKIDs = []string{r.KID} },
		"crl serial": func(r *license.SignRequest) { r.CRL.Serial = 0 },
		"grace":      func(r *license.SignRequest) { r.Licenses[0].GraceDays = 365 },
	}
	for name, mut := range cases {
		b, _ := json.Marshal(base)
		var r license.SignRequest
		_ = json.Unmarshal(b, &r)
		mut(&r)
		if err := r.Validate(now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := license.ParseSignRequest([]byte(`{"format":"etos-sign-request-v1","batch":"B0001","kid":"etos-prod-2027","extra":1}`), now); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestLogAndDuplicate(t *testing.T) {
	kf, _, _ := Generate("etos-prod-2027", []byte(pass), now)
	priv, _ := kf.Open([]byte(pass))
	raw, req := request(t, kf.KID)
	out, _ := SignBatch(req, raw, kf.KID, priv, now)
	log := filepath.Join(t.TempDir(), "signer-log.jsonl")
	if Signed(log, out.RequestSHA256) {
		t.Fatal("empty log reports signed")
	}
	if err := AppendLog(log, req, out); err != nil {
		t.Fatal(err)
	}
	if !Signed(log, out.RequestSHA256) {
		t.Fatal("duplicate not detected")
	}
	if fi, _ := os.Stat(log); fi.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "PRIVATE") || !strings.Contains(string(b), "LIC-20261102-0002") {
		t.Fatalf("log content: %s", b)
	}
	s := Summary(req)
	for _, want := range []string{"B0012", "某某公司", "LIC-20261102-0001", "吊销列表：序号 4", "退款"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}
