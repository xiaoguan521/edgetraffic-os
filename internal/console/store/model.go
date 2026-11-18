package store

//go:generate go run ./gen -out ./query

import "time"

// 模型定义。修改任何字段或索引都必须在 migrations.go 中新增一条迁移（TestModelsMatchMigrations 会检查）。

// 角色（规格书 7.1 M11，MVP 子集）。
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleReleaser = "releaser"
	RoleViewer   = "viewer"
)

type User struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	Username     string `gorm:"uniqueIndex;size:64" json:"username"`
	PasswordHash string `json:"-"`
	Role         string `gorm:"size:16" json:"role"`
	CreatedAt    time.Time
	LastLoginAt  *time.Time `json:"last_login_at"`

	// 账号安全（等保 2.0 三级，P9）
	Source             string     `gorm:"size:16" json:"source"` // local | ldap | oidc（空视为 local）
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"must_change_password"`
	PasswordChangedAt  *time.Time `json:"password_changed_at"`
	PasswordHistory    string     `json:"-"` // 最近几次口令哈希（JSON）
	FailedLogins       int        `json:"failed_logins"`
	LockedUntil        *time.Time `json:"locked_until"`
	// 会话版本：会话令牌携带签发时的值，与此处不一致即失效。退出、改口令、重置口令时递增。
	TokenVersion int `gorm:"not null;default:0" json:"-"`
}

type NodeGroup struct {
	ID                uint   `gorm:"primaryKey" json:"id"`
	Name              string `gorm:"uniqueIndex;size:64" json:"name"`
	Description       string `json:"description"`
	AllowedCIDRs      string `gorm:"column:allowed_cidrs" json:"allowed_cidrs"` // 逗号分隔
	RealIPFrom        string `json:"real_ip_from"`                              // 逗号分隔
	CurrentRevisionID string `gorm:"size:16" json:"current_revision_id"`
	// InstanceProfile 是节点组的实例档案（render.Profile 的 JSON；空串为默认实例），设计 0001 第 3.4、5.2 节
	InstanceProfile string `json:"instance_profile"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Node struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	GroupID         uint       `gorm:"index" json:"group_id"`
	Name            string     `json:"name"`
	Hostname        string     `json:"hostname"`
	HostKey         string     `gorm:"index;size:64" json:"host_key"` // /etc/machine-id 的 sha256（agent 注册与 Hello 上报），同机实例的冲突检查以它为单位
	RemoteAddr      string     `json:"remote_addr"`
	CertSerial      string     `json:"-"`
	Status          string     `gorm:"size:16" json:"status"` // online | offline | revoked
	AgentVersion    string     `json:"agent_version"`
	Arch            string     `gorm:"size:16" json:"arch"` // agent 上报（amd64 | arm64），旧版 agent 为空
	SyappVersion    string     `json:"syapp_version"`
	RunningVersion  string     `gorm:"size:32" json:"running_version"` // 本实例 worker 实际运行的版本（同机实例共用二进制时可能与 syapp_version 不同）
	SyappSHA256     string     `json:"syapp_sha256"`
	TLSLib          string     `gorm:"size:16" json:"tls_lib"`   // syedge -V 中的 TLS 库：openssl | tongsuo（国密变体）
	LuaAddon        string     `gorm:"size:40" json:"lua_addon"` // 已安装的 syedge-lua 附加包：<包版本>/<基础变体>，未安装为空（P10）
	AppliedRevision string     `gorm:"size:16" json:"applied_revision"`
	TreeHash        string     `json:"tree_hash"`
	Drifted         bool       `json:"drifted"`
	CPUPercent      float64    `json:"cpu_percent"`
	MemUsedBytes    uint64     `json:"mem_used_bytes"`
	ActiveConns     uint64     `json:"active_connections"`
	RequestsTotal   uint64     `json:"requests_total"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type EnrollToken struct {
	ID        uint   `gorm:"primaryKey"`
	TokenHash string `gorm:"uniqueIndex;size:64"`
	GroupID   uint
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedBy string
	CreatedAt time.Time
}

type Site struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	GroupID   uint   `gorm:"uniqueIndex:idx_site_group_name" json:"group_id"`
	Name      string `gorm:"uniqueIndex:idx_site_group_name;size:64" json:"name"`
	Spec      string `json:"-"` // render.Site 的 JSON
	Enabled   bool   `json:"enabled"`
	Version   int    `json:"version"`
	UpdatedBy string `json:"updated_by"`
	CreatedAt time.Time
	UpdatedAt time.Time `json:"updated_at"`
}

type Upstream struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	GroupID   uint   `gorm:"uniqueIndex:idx_up_group_name" json:"group_id"`
	Name      string `gorm:"uniqueIndex:idx_up_group_name;size:64" json:"name"`
	Spec      string `json:"-"` // render.UpstreamGroup 的 JSON
	UpdatedBy string `json:"updated_by"`
	CreatedAt time.Time
	UpdatedAt time.Time `json:"updated_at"`
}

// Revision 是不可变的配置版本。
type Revision struct {
	Seq       uint      `gorm:"primaryKey" json:"-"`
	ID        string    `gorm:"uniqueIndex;size:16" json:"id"`
	GroupID   uint      `gorm:"index" json:"group_id"`
	ParentID  string    `gorm:"size:16" json:"parent_id"`
	Digest    string    `json:"digest"`
	Bundle    []byte    `json:"-"`
	Manifest  string    `json:"-"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type Deployment struct {
	ID         uint             `gorm:"primaryKey" json:"id"`
	GroupID    uint             `gorm:"index" json:"group_id"`
	RevisionID string           `gorm:"size:16" json:"revision_id"`
	Kind       string           `gorm:"size:16" json:"kind"`   // publish | rollback | align
	Status     string           `gorm:"size:16" json:"status"` // APPLYING | APPLIED | PARTIAL | FAILED
	Note       string           `json:"note"`
	CreatedBy  string           `json:"created_by"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt *time.Time       `json:"finished_at"`
	Nodes      []DeploymentNode `gorm:"foreignKey:DeploymentID" json:"nodes,omitempty"`
}

// ReleaseRequest 是一次待审批的发布（规格书 12.3 P7 发布审批）：提交时冻结版本，批准后原样下发。
type ReleaseRequest struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	GroupID        uint       `gorm:"index" json:"group_id"`
	RevisionID     string     `gorm:"size:16" json:"revision_id"`      // 提交时生成的不可变版本
	BaseRevisionID string     `gorm:"size:16" json:"base_revision_id"` // 提交时组的线上版本
	Digest         string     `json:"digest"`
	Status         string     `gorm:"index;size:16" json:"status"` // pending | approved | rejected | cancelled | stale
	Note           string     `json:"note"`
	RequestedBy    string     `gorm:"size:64" json:"requested_by"`
	RequestedAt    time.Time  `json:"requested_at"`
	ReviewedBy     string     `gorm:"size:64" json:"reviewed_by"`
	ReviewedAt     *time.Time `json:"reviewed_at"`
	ReviewComment  string     `json:"review_comment"`
	DeploymentID   uint       `json:"deployment_id"`
}

// Snippet 是自定义配置片段（规格书 12.7 路径 A）。任何变更都先进入 Pending*，由另一人批准后才成为 Content 并参与渲染。
type Snippet struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	GroupID       uint       `gorm:"uniqueIndex:idx_snippet_slot" json:"group_id"`
	SiteID        uint       `gorm:"uniqueIndex:idx_snippet_slot" json:"site_id"`             // http 片段为 0
	Scope         string     `gorm:"uniqueIndex:idx_snippet_slot;size:16" json:"scope"`       // http | server | location
	RoutePath     string     `gorm:"uniqueIndex:idx_snippet_slot;size:255" json:"route_path"` // location 片段对应的路由路径
	Name          string     `gorm:"uniqueIndex:idx_snippet_slot;size:32" json:"name"`        // http 片段名
	Content       string     `json:"content"`                                                 // 已批准、生效中的规范化文本（空表示尚未生效）
	ContentSHA256 string     `gorm:"column:content_sha256;size:64" json:"content_sha256"`
	Pending       string     `json:"pending"` // 待审批的新内容（规范化后）
	PendingDelete bool       `json:"pending_delete"`
	PendingBy     string     `gorm:"size:64" json:"pending_by"`
	PendingAt     *time.Time `json:"pending_at"`
	PendingNote   string     `json:"pending_note"`
	Origin        string     `gorm:"size:32" json:"origin"` // manual | import | drift
	ApprovedBy    string     `gorm:"size:64" json:"approved_by"`
	ApprovedAt    *time.Time `json:"approved_at"`
	CreatedBy     string     `gorm:"size:64" json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ConfigImport 是一次配置导入（规格书 12.4）：保存完整报告（不含证书与私钥）供下载与审计。
type ConfigImport struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	GroupID      uint       `gorm:"index" json:"group_id"`
	Source       string     `gorm:"size:16" json:"source"` // dump | archive
	FileName     string     `json:"file_name"`
	Report       string     `json:"-"`
	ReportSHA256 string     `gorm:"column:report_sha256;size:64" json:"report_sha256"`
	Sites        int        `json:"sites"`
	Full         int        `json:"full"`
	Partial      int        `json:"partial"`
	Failed       int        `json:"failed"`
	Issues       int        `json:"issues"`
	CreatedBy    string     `gorm:"size:64" json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	AppliedBy    string     `gorm:"size:64" json:"applied_by"`
	AppliedAt    *time.Time `json:"applied_at"`
	Result       string     `json:"-"` // 应用结果 JSON
}

// APIToken 是开放 API 的访问令牌（规格书 9 / P9，授权功能 api_token）。只保存哈希，明文只在创建时返回一次。
type APIToken struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	Name       string     `gorm:"size:64" json:"name"`
	Username   string     `gorm:"index;size:64" json:"username"` // 令牌代表的用户
	Role       string     `gorm:"size:16" json:"role"`           // 不高于该用户角色
	TokenHash  string     `gorm:"uniqueIndex;size:64" json:"-"`
	Prefix     string     `gorm:"size:16" json:"prefix"` // 便于识别的前 12 个字符
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `gorm:"size:64" json:"last_used_ip"`
	Revoked    bool       `json:"revoked"`
	CreatedBy  string     `gorm:"size:64" json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

// AlertChannel 是告警通道（规格书 7.1 M9）。密钥类字段加密存储，接口从不返回。
type AlertChannel struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"uniqueIndex;size:64" json:"name"`
	Type      string    `gorm:"size:16" json:"type"` // webhook | wecom | dingtalk | email
	URL       string    `json:"url"`                 // webhook / 机器人地址（email 为 smtp://host:port）
	SecretEnc string    `json:"-"`                   // 钉钉加签密钥 / SMTP 口令（加密）
	SMTPUser  string    `gorm:"column:smtp_user" json:"smtp_user"`
	MailFrom  string    `json:"mail_from"`
	MailTo    string    `json:"mail_to"` // 逗号分隔
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `gorm:"size:64" json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AlertRule 是内置告警规则的配置（离线、5xx 率、证书到期、配置漂移）。
type AlertRule struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Kind       string    `gorm:"uniqueIndex;size:32" json:"kind"`
	Enabled    bool      `json:"enabled"`
	Threshold  float64   `json:"threshold"`
	ChannelIDs string    `json:"channel_ids"` // 逗号分隔
	UpdatedBy  string    `gorm:"size:64" json:"updated_by"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AlertEvent 是一次告警（同一对象同一规则同时只有一条 firing）。
type AlertEvent struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	Kind       string     `gorm:"size:32" json:"kind"`
	ObjectKey  string     `gorm:"index;size:128" json:"object_key"`
	Message    string     `json:"message"`
	Status     string     `gorm:"index;size:16" json:"status"` // firing | resolved
	FiredAt    time.Time  `json:"fired_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Notify     string     `json:"notify"` // 最近一次通知结果
}

// NodeDrift 是节点最近一次上报的漂移差异（规格书 12.7 路径 B）。节点恢复一致后删除。
type NodeDrift struct {
	NodeID     uint      `gorm:"primaryKey" json:"node_id"`
	RevisionID string    `gorm:"size:16" json:"revision_id"`
	TreeHash   string    `gorm:"size:80" json:"tree_hash"`
	Files      string    `json:"-"` // JSON：路径、变化、哈希、大小、内容（私钥与超限文件无内容）
	ReportedAt time.Time `json:"reported_at"`
}

// DataplanePackage 是版本库中的一个厂商签名升级包（规格书 12.6）。导入时验签，包内容存库。
type DataplanePackage struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Name       string    `gorm:"uniqueIndex:idx_pkg;size:32" json:"name"` // syedge | syedge-agent
	Version    string    `gorm:"uniqueIndex:idx_pkg;size:32" json:"version"`
	Arch       string    `gorm:"uniqueIndex:idx_pkg;size:16" json:"arch"`
	Variant    string    `gorm:"uniqueIndex:idx_pkg;size:16" json:"variant"`
	SHA256     string    `gorm:"column:sha256;size:64" json:"sha256"`
	Size       int64     `json:"size"`
	KID        string    `gorm:"column:kid;size:40" json:"kid"`
	Sig        string    `json:"-"`
	Data       []byte    `json:"-"`
	UploadedBy string    `gorm:"size:64" json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// DataplaneUpgrade 是一次节点组的分批热升级（先 1 台、再 10%、再全量，任一节点失败即停止）。
type DataplaneUpgrade struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	GroupID    uint       `gorm:"index" json:"group_id"`
	Version    string     `gorm:"size:32" json:"version"`
	Variant    string     `gorm:"size:16" json:"variant"`
	Status     string     `gorm:"size:16" json:"status"` // running | done | failed
	Nodes      string     `json:"-"`                     // JSON：逐节点批次与结果
	Note       string     `json:"note"`
	CreatedBy  string     `gorm:"size:64" json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// StreamService 是节点组内的一个四层代理服务（P9，授权功能 stream）。
type StreamService struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	GroupID   uint      `gorm:"uniqueIndex:idx_stream_group_name" json:"group_id"`
	Name      string    `gorm:"uniqueIndex:idx_stream_group_name;size:64" json:"name"`
	Spec      string    `json:"-"` // render.StreamProxy 的 JSON
	Enabled   bool      `json:"enabled"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CanaryRelease 是一次灰度发布（P9，授权功能 canary）：按批次发布并观察 5xx 率，超阈值自动回滚。
type CanaryRelease struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	GroupID        uint       `gorm:"index" json:"group_id"`
	RevisionID     string     `gorm:"size:16" json:"revision_id"`
	BaseRevisionID string     `gorm:"size:16" json:"base_revision_id"`
	Status         string     `gorm:"size:16" json:"status"` // running | done | rolled_back | failed
	Options        string     `json:"-"`                     // JSON
	Batches        string     `json:"-"`                     // JSON：每批节点、发布记录与观察结果
	Note           string     `json:"note"`
	Message        string     `json:"message"`
	CreatedBy      string     `gorm:"size:64" json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

// StaticBundle 是一个静态资源包（P9 静态资源发布）：上传后不可变，被站点静态路由引用，随配置版本原子切换。
type StaticBundle struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	GroupID    uint      `gorm:"index" json:"group_id"`
	Name       string    `gorm:"size:128" json:"name"`
	SHA256     string    `gorm:"column:sha256;size:64" json:"sha256"`
	Size       int64     `json:"size"`
	Files      int       `json:"files"`
	Data       []byte    `json:"-"` // 规范化后的 tar.gz
	UploadedBy string    `gorm:"size:64" json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type DeploymentNode struct {
	ID           uint       `gorm:"primaryKey" json:"-"`
	DeploymentID uint       `gorm:"index" json:"-"`
	NodeID       uint       `json:"node_id"`
	NodeName     string     `json:"node_name"`
	Status       string     `gorm:"size:16" json:"status"` // PENDING | APPLYING | APPLIED | INVALID | ROLLED_BACK | DEGRADED | REJECTED | CONFLICT | OFFLINE | TIMEOUT
	Message      string     `json:"message"`
	Stderr       string     `json:"stderr"`
	FinishedAt   *time.Time `json:"finished_at"`
}

type MetricMinute struct {
	ID       uint   `gorm:"primaryKey"`
	NodeID   uint   `gorm:"index:idx_metric,priority:2"`
	Site     string `gorm:"size:255"`
	Minute   int64  `gorm:"index:idx_metric,priority:1"`
	Requests uint64
	S2xx     uint64
	S3xx     uint64
	S4xx     uint64
	S5xx     uint64
	P50ms    float64
	P95ms    float64
	Bytes    uint64
}

type AuditLog struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	TS         time.Time `gorm:"index" json:"ts"`
	Username   string    `json:"username"`
	IP         string    `json:"ip"`
	Action     string    `json:"action"`
	ObjectType string    `json:"object_type"`
	ObjectID   string    `json:"object_id"`
	Detail     string    `json:"detail"`
	Result     string    `json:"result"`
	PrevHash   string    `json:"prev_hash"`
	Hash       string    `json:"hash"`
}

type LicenseStore struct {
	ID         uint `gorm:"primaryKey"`
	Raw        string
	ImportedBy string
	ImportedAt time.Time
}

// GM 报告节点是否运行国密变体（铜锁构建，支持 TLCP 双证书）。
func (n *Node) GM() bool { return n.TLSLib == "tongsuo" }

// IPSet 是节点组的命名 IP 集（地域库），被规则积木的 ip_set 条件引用（P10）。
type IPSet struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	GroupID   uint      `gorm:"uniqueIndex:idx_ipset_group_name" json:"group_id"`
	Name      string    `gorm:"uniqueIndex:idx_ipset_group_name;size:64" json:"name"`
	Label     string    `gorm:"size:128" json:"label"`
	Source    string    `gorm:"size:32" json:"source"` // manual | rir:<国家代码>
	CIDRs     string    `gorm:"column:cidrs" json:"-"` // JSON 数组
	Count     int       `json:"count"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// KindSM2 是国密双证书的 Kind。
const KindSM2 = "sm2"

// Certificate 是证书库中的一张证书（规格书 7.3 证书管理）。私钥以 secretbox 加密保存，永不出现在 API 响应中。
type Certificate struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"uniqueIndex;size:64" json:"name"`
	Domains     string    `json:"-"` // JSON 数组：证书覆盖的域名（SAN）
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `gorm:"index" json:"not_after"`
	Fingerprint string    `gorm:"size:64" json:"fingerprint"`
	CertPEM     string    `json:"-"`
	KeyEnc      string    `json:"-"`
	Source      string    `gorm:"size:16" json:"source"` // upload | migrated | acme
	AutoRenew   bool      `json:"auto_renew"`            // ACME 证书到期前 30 天自动续期
	// Kind 为 sm2 时是国密 TLCP 双证书：CertPEM / KeyEnc 存签名证书与私钥，Enc* 存加密证书与私钥；
	// 域名取自签名证书，NotAfter 取两张证书中较早的到期时间。
	Kind           string    `gorm:"size:8" json:"kind"` // 空：标准证书 | sm2
	EncCertPEM     string    `json:"-"`
	EncKeyEnc      string    `json:"-"`
	EncFingerprint string    `gorm:"size:64" json:"enc_fingerprint,omitempty"`
	UpdatedBy      string    `json:"updated_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AcmeOrder 是一次 ACME 签发 / 续期（规格书 P6-4）。
type AcmeOrder struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	Name       string     `gorm:"size:64" json:"name"`
	CertID     uint       `gorm:"index" json:"cert_id"`  // 0 表示新签发
	Domains    string     `json:"-"`                     // JSON 数组
	Status     string     `gorm:"size:16" json:"status"` // pending | valid | failed
	Step       string     `json:"step"`                  // 当前步骤（展示用）
	Error      string     `json:"error"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// AcmeChallenge 是进行中的 HTTP-01 挑战，渲染进节点配置的挑战片段。
type AcmeChallenge struct {
	ID        uint   `gorm:"primaryKey"`
	OrderID   uint   `gorm:"index"`
	Domain    string `gorm:"size:255"`
	Token     string `gorm:"uniqueIndex;size:128"`
	KeyAuth   string `gorm:"size:255"`
	CreatedAt time.Time
}

type SystemKV struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string
}

// Models 返回全部模型（gorm/gen 生成查询代码与迁移守卫测试使用）。
func Models() []any {
	return []any{&User{}, &NodeGroup{}, &Node{}, &EnrollToken{}, &Site{}, &Upstream{}, &Revision{}, &Deployment{},
		&DeploymentNode{}, &ReleaseRequest{}, &Snippet{}, &ConfigImport{}, &APIToken{}, &AlertChannel{}, &AlertRule{}, &AlertEvent{}, &NodeDrift{}, &DataplanePackage{}, &DataplaneUpgrade{}, &StreamService{}, &CanaryRelease{}, &StaticBundle{}, &IPSet{}, &MetricMinute{}, &AuditLog{}, &LicenseStore{}, &Certificate{}, &AcmeOrder{}, &AcmeChallenge{}, &SystemKV{}}
}
