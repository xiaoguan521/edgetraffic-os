package pkgsig

// builtinKeys 是正式的升级包签名公钥（离线签名机上 syapp-license-signer keygen -kid etos-pkg-<年份> 生成）。
// 只放公钥。密钥仪式完成前为空：正式版本的发布门禁 TestReleasePackageKeys 会失败。
var builtinKeys = map[string]string{}
