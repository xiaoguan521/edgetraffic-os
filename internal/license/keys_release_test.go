//go:build release

package license

import "testing"

// 正式版本（-tags release）的门禁：不含演示密钥，且至少内置一把正式签名密钥的公钥。
// release 工作流在 v* 标签上运行它；在生成正式密钥并把公钥写入 keys.go 之前，打 v* 标签会在这里失败。
func TestReleaseKeyRing(t *testing.T) {
	kr := BuiltinKeyRing()
	if _, ok := kr[DemoKeyID]; ok {
		t.Fatal("release build must not contain the demo signing key")
	}
	if len(ProductionKIDs()) == 0 {
		t.Fatal("release build has no production signing key: generate one with syedge-license-signer keygen and add its public key to internal/license/keys.go (docs/license-ops.md)")
	}
	// 主密钥丢失或泄露时，客户控制台必须已经信任备用密钥（docs/license-ops.md 第 3.3 节）
	if len(ProductionKIDs()) < 2 {
		t.Fatal("release build needs a standby signing key (etos-prod-<year>-b) besides the primary one (docs/license-ops.md §3.3)")
	}
}
