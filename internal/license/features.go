package license

// Feature 是 License 中 features 数组的取值（规格书 8.2）。
// 版本名只用于展示；功能是否可用只看 License 里列出的 features。
type Feature string

const (
	FeatureCore            Feature = "core"
	FeatureAPIToken        Feature = "api_token"
	FeatureBatchRelease    Feature = "batch_release"
	FeatureReleaseApproval Feature = "release_approval"
	FeatureSSO             Feature = "sso"
	FeatureAuditExport     Feature = "audit_export"
	FeatureAlertIM         Feature = "alert_im"
	FeatureStream          Feature = "stream"
	FeatureHA              Feature = "ha"
	FeatureCanary          Feature = "canary"
	FeatureGMTLS           Feature = "gm_tls"
	FeatureLua             Feature = "lua" // P10：Lua 规则（JWT 校验，需要节点装 syedge-lua 附加包）；原生规则积木属于 core
)

// Edition 是展示用的版本名。
type Edition string

const (
	EditionTrial        Edition = "trial"
	EditionStandard     Edition = "standard"
	EditionProfessional Edition = "professional"
	EditionEnterprise   Edition = "enterprise"
)
