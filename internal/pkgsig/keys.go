package pkgsig

// builtinKeys 是正式的升级包签名公钥（离线签名机上 syedge-license-signer keygen -kid etos-pkg-<年份> 生成）。
// 只放公钥（2026-11-06 密钥仪式）。
// 仪式同时生成主密钥与备用密钥（etos-pkg-<年份>-b，分开保管、平时不用），两把公钥都内置：
// 主密钥丢失或泄露时改用备用密钥签名，已部署的节点无需先升级就能验证（docs/license-ops.md 第 3.3 节）。
var builtinKeys = map[string]string{
	"etos-pkg-2027":   "cHvZbmzVruA+nBpNeKLv8BZ4sHjdUS9W6n0XrLmIsn0=",
	"etos-pkg-2027-b": "JOM6HsRnAqo0ldQVLEkeIx5ISmTic4e6ZUIJko5VgUU=",
}

// revokedKIDs 是已吊销的升级包签名密钥（私钥泄露后加入）。这里的 kid 即使仍在 builtinKeys 中也不会被信任。
var revokedKIDs = map[string]bool{}
