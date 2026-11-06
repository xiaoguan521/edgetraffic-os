package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

// 2026-11-06 密钥仪式记录中的公钥指纹（双人抄写核对）。内置公钥被改动或遗漏时失败；新增密钥时同时登记在这里。
func TestCeremonyFingerprints(t *testing.T) {
	want := map[string]string{
		"etos-prod-2027":   "0BAE 5F00 6427 28DE 9B30 79E1 CF3A 3C62 EBB1 6ADF D3ED 556A 8968 997C 5B4A DF79",
		"etos-prod-2027-b": "9723 D2E3 C988 C477 4776 29DF 07DF F1A0 97DA 7544 2279 138B C611 B595 865F 230B",
	}
	for kid, fp := range want {
		b, err := base64.StdEncoding.DecodeString(builtinKeys[kid])
		if err != nil || len(b) != ed25519.PublicKeySize {
			t.Fatalf("%s: missing or malformed public key", kid)
		}
		if got := KeyFingerprint(b); got != fp {
			t.Errorf("%s: fingerprint %s, ceremony record %s", kid, got, fp)
		}
	}
	n := len(builtinKeys)
	if _, ok := builtinKeys[DemoKeyID]; ok {
		n-- // 演示构建由 keys_demo.go 加入
	}
	if n != len(want) {
		t.Errorf("builtinKeys has %d keys, ceremony record lists %d", n, len(want))
	}
}
