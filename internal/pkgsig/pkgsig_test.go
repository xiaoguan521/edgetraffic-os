package pkgsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	data := []byte("package bytes")
	s, err := Sign(priv, "etos-ci-pkg-test", "syapp", "1.30.6-1", "amd64", "standard", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(s, data, keys); err != nil {
		t.Fatal(err)
	}
	// 内容、版本、架构、变体、包名任何一项被改动都失败
	for name, mut := range map[string]func(x *Sig){
		"version": func(x *Sig) { x.Version = "1.30.7" },
		"arch":    func(x *Sig) { x.Arch = "arm64" },
		"variant": func(x *Sig) { x.Variant = "gmssl" },
		"name":    func(x *Sig) { x.Name = "syapp-agent" },
		"kid":     func(x *Sig) { x.KID = "etos-pkg-2099" },
	} {
		c := *s
		mut(&c)
		if err := Verify(&c, data, keys); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: tampering not detected", name)
		}
	}
	if err := Verify(s, []byte("other"), keys); !errors.Is(err, ErrSignature) {
		t.Fatal("content tampering not detected")
	}
	// 非法字段在签名时就被拒绝
	if _, err := Sign(priv, "etos-ci-pkg-test", "syapp", "1.30.6;rm", "amd64", "standard", data); err == nil {
		t.Fatal("bad version accepted")
	}
	if _, err := Sign(priv, "etos-prod-2027", "syapp", "1.30.6", "amd64", "standard", data); err == nil {
		t.Fatal("license key id accepted for packages")
	}
}

func TestDemoKeyParses(t *testing.T) {
	for kid := range demoKeys {
		if _, ok := Keys()[kid]; !ok {
			t.Fatalf("demo key %s does not parse", kid)
		}
	}
}

// 升级包签名密钥泄露后：加入 revokedKIDs 的 kid 不再进入公钥环，即使公钥仍在 builtinKeys 中。
func TestRevokedPackageKeyExcluded(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	oldKeys, oldRev := builtinKeys, revokedKIDs
	defer func() { builtinKeys, revokedKIDs = oldKeys, oldRev }()
	b64 := base64.StdEncoding.EncodeToString(pub)
	builtinKeys = map[string]string{"etos-pkg-2027": b64, "etos-pkg-2027-b": b64}
	revokedKIDs = map[string]bool{"etos-pkg-2027": true}
	k := Keys()
	if _, ok := k["etos-pkg-2027"]; ok {
		t.Fatal("revoked package key still trusted")
	}
	if _, ok := k["etos-pkg-2027-b"]; !ok {
		t.Fatal("standby package key missing")
	}
}
