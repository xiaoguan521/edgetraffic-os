package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"time"
)

// 内置的厂商公钥（按 kid）。私钥只存在于厂商授权后台，绝不进入代码仓库。
//
// etos-demo-2026：MVP / 演示用密钥，私钥由项目负责人离线保管。
// 正式发布前替换为 KMS 中的生产密钥，并从此表移除演示密钥（规格书 8.7）。
var builtinKeys = map[string]string{
	"etos-demo-2026": "2s66YtpjpEGRSzQU7Sus8hvAbDXp6yAQjOI3y1AeJlU=",
}

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
