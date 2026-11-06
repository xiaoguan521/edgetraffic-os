package pkgsig

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// 2026-11-06 密钥仪式记录中的升级包公钥指纹（双人抄写核对）。
func TestCeremonyFingerprints(t *testing.T) {
	want := map[string]string{
		"etos-pkg-2027":   "DE19 0DB5 DBC4 2809 1E6A 111B DC8E 1D49 6027 BF0C CB16 A91F 40BB 9D46 2806 386F",
		"etos-pkg-2027-b": "26A3 0C39 170B AADC 4C93 1125 8E1A 1F50 D588 58BB B473 75EF 5604 BE35 EC6D 7C6D",
	}
	for kid, fp := range want {
		b, err := base64.StdEncoding.DecodeString(builtinKeys[kid])
		if err != nil || len(b) != 32 {
			t.Fatalf("%s: missing or malformed public key", kid)
		}
		h := strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(b)))
		var g []string
		for i := 0; i < len(h); i += 4 {
			g = append(g, h[i:i+4])
		}
		if got := strings.Join(g, " "); got != fp {
			t.Errorf("%s: fingerprint %s, ceremony record %s", kid, got, fp)
		}
	}
	if n := len(builtinKeys); n != len(want) {
		t.Errorf("builtinKeys has %d keys, ceremony record lists %d", n, len(want))
	}
}
