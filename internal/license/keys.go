package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"time"
)

// builtinKeys 是正式签名密钥的公钥（按 kid）。私钥只存在于厂商的离线签名机（规格书 8.8.1），
// 绝不进入代码仓库、CI 或授权后台。新增或轮换密钥的步骤见 docs/license-ops.md。
//
// 演示密钥在 keys_demo.go 中，正式版本（-tags release）不包含它。
var builtinKeys = map[string]string{}

// revokedKIDs 是随控制台版本内置的已吊销签名密钥（私钥泄露或退役后加入，规格书 8.7）。
// 这里的 kid 即使出现在 builtinKeys 中也不会进入公钥环。
var revokedKIDs = map[string]bool{}

// ciKey 只在 CI 端到端测试的构建中通过 -ldflags 注入（每次运行临时生成，用完即弃），
// 正式与演示构建中为空。对应的 kid 固定为 CIKeyID。
var ciKey string

// CIKeyID 是 CI 临时密钥的编号。
const CIKeyID = "etos-ci-ephemeral"

// BuiltinKeyRing 返回内置公钥环。
func BuiltinKeyRing() KeyRing {
	kr := KeyRing{}
	if b, err := base64.StdEncoding.DecodeString(ciKey); err == nil && len(b) == ed25519.PublicKeySize {
		kr[CIKeyID] = ed25519.PublicKey(b)
	}
	for kid, b64 := range builtinKeys {
		if revokedKIDs[kid] {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err == nil && len(b) == ed25519.PublicKeySize {
			kr[kid] = ed25519.PublicKey(b)
		}
	}
	return kr
}

// AgentCheck 是 agent 侧的独立校验（规格书 8.6）：只验签名与有效期，不涉及控制台安装绑定。
// 返回 License 是否允许新的发布；任何结果都不会影响 syapp 的运行。
func AgentCheck(raw string, keys KeyRing, now time.Time) (writable bool, reason string) {
	p, err := Verify(raw, keys)
	if err != nil {
		return false, err.Error()
	}
	if now.Before(p.NotBefore) {
		return false, "license not yet valid"
	}
	if now.After(p.ExpiresAt.Add(time.Duration(p.GraceDays) * 24 * time.Hour)) {
		return false, "license expired"
	}
	return true, ""
}

// DemoKeyID 是演示密钥的编号。
const DemoKeyID = "etos-demo-2026"

// ProductionKIDs 返回内置的正式签名密钥编号（不含演示与 CI 临时密钥、不含已吊销的）。
func ProductionKIDs() []string {
	var out []string
	for kid := range BuiltinKeyRing() {
		if kid != DemoKeyID && kid != CIKeyID {
			out = append(out, kid)
		}
	}
	return out
}
