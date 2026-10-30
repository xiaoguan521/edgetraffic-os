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
}

type NodeGroup struct {
	ID                uint   `gorm:"primaryKey" json:"id"`
	Name              string `gorm:"uniqueIndex;size:64" json:"name"`
	Description       string `json:"description"`
	AllowedCIDRs      string `gorm:"column:allowed_cidrs" json:"allowed_cidrs"` // 逗号分隔
	RealIPFrom        string `json:"real_ip_from"`                              // 逗号分隔
	CurrentRevisionID string `gorm:"size:16" json:"current_revision_id"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Node struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	GroupID         uint       `gorm:"index" json:"group_id"`
	Name            string     `json:"name"`
	Hostname        string     `json:"hostname"`
	RemoteAddr      string     `json:"remote_addr"`
	CertSerial      string     `json:"-"`
	Status          string     `gorm:"size:16" json:"status"` // online | offline | revoked
	AgentVersion    string     `json:"agent_version"`
	SyappVersion    string     `json:"syapp_version"`
	SyappSHA256     string     `json:"syapp_sha256"`
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
	UpdatedBy   string    `json:"updated_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
		&DeploymentNode{}, &MetricMinute{}, &AuditLog{}, &LicenseStore{}, &Certificate{}, &AcmeOrder{}, &AcmeChallenge{}, &SystemKV{}}
}
