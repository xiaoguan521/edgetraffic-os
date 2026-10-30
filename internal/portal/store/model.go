// Package store 是厂商授权后台（syapp-license-server serve，规格书 8.8 / 8.8.1）的数据层。
//
// 授权后台只保存公钥与业务数据，从不保存任何签名私钥。修改模型必须在 migrations.go 末尾新增迁移
// （TestModelsMatchMigrations 检查）；业务代码通过 gorm/gen 生成的 query 包访问数据库。
package store

//go:generate go run ./gen -out ./query

import "time"

// 角色。一个用户可以有多个角色；admin 拥有全部权限。
const (
	RoleAdmin    = "admin"    // 用户、签名密钥登记、全部操作
	RoleSales    = "sales"    // 客户、订单、License 草稿与提交
	RoleBusiness = "business" // 审批、吊销
	RoleSigner   = "signer"   // 导出签名批次、导入签名结果
)

// License 状态。
const (
	StatusDraft     = "draft"
	StatusSubmitted = "submitted"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusBatched   = "batched" // 已导出到签名批次，等待离线签名
	StatusIssued    = "issued"
	StatusRevoked   = "revoked"
)

type User struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	Username     string     `gorm:"uniqueIndex;size:64" json:"username"`
	DisplayName  string     `gorm:"size:64" json:"display_name"`
	PasswordHash string     `json:"-"`
	Roles        string     `gorm:"size:64" json:"roles"` // 逗号分隔
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
}

// SigningKey 是登记的签名公钥（私钥在离线签名机上）。
type SigningKey struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	KID         string    `gorm:"column:kid;uniqueIndex;size:40" json:"kid"`
	PublicKey   string    `gorm:"size:64" json:"public_key"`
	Fingerprint string    `gorm:"size:80" json:"fingerprint"`
	Status      string    `gorm:"size:16" json:"status"` // active | retired | revoked
	Note        string    `json:"note"`
	CreatedBy   string    `gorm:"size:64" json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Customer struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Code       string    `gorm:"uniqueIndex;size:32" json:"code"`
	Name       string    `gorm:"size:128" json:"name"`
	Contact    string    `gorm:"size:64" json:"contact"`
	Email      string    `gorm:"size:128" json:"email"`
	Phone      string    `gorm:"size:32" json:"phone"`
	SalesOwner string    `gorm:"size:64" json:"sales_owner"`
	Note       string    `json:"note"`
	CreatedBy  string    `gorm:"size:64" json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Order struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	OrderNo    string    `gorm:"uniqueIndex;size:32" json:"order_no"`
	CustomerID uint      `gorm:"index" json:"customer_id"`
	Edition    string    `gorm:"size:16" json:"edition"`
	Nodes      int       `json:"nodes"`
	Sites      int       `json:"sites"`
	Days       int       `json:"days"`
	GraceDays  int       `json:"grace_days"`
	Amount     string    `gorm:"size:32" json:"amount"`
	ContractNo string    `gorm:"size:64" json:"contract_no"`
	Note       string    `json:"note"`
	CreatedBy  string    `gorm:"size:64" json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// License 是一张 License 从草稿到签发 / 吊销的完整记录。
type License struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	LicenseID       string     `gorm:"uniqueIndex;size:64" json:"license_id"`
	CustomerID      uint       `gorm:"index" json:"customer_id"`
	OrderID         uint       `gorm:"index" json:"order_id"`
	ParentLicenseID string     `gorm:"size:64" json:"parent_license_id"` // 续期 / 扩容时的原 License
	Status          string     `gorm:"index;size:16" json:"status"`
	Payload         string     `json:"-"` // license.Payload 的 JSON（kid 在导出批次时填写）
	InstallationID  string     `gorm:"index;size:64" json:"installation_id"`
	Binding         string     `gorm:"size:24" json:"binding"`
	ExpiresAt       time.Time  `gorm:"index" json:"expires_at"`
	Raw             string     `json:"-"` // 签好的 License
	BatchID         uint       `gorm:"index" json:"batch_id"`
	SubmittedBy     string     `gorm:"size:64" json:"submitted_by"`
	ApprovedBy      string     `gorm:"size:64" json:"approved_by"`
	RejectReason    string     `json:"reject_reason"`
	IssuedAt        *time.Time `json:"issued_at"`
	RevokedAt       *time.Time `json:"revoked_at"`
	RevokeReason    string     `json:"revoke_reason"`
	CreatedBy       string     `gorm:"size:64" json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Batch 是一个签名批次：导出的签名请求与导入的签名结果都完整保存。
type Batch struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	BatchNo       string     `gorm:"uniqueIndex;size:16" json:"batch_no"`
	KID           string     `gorm:"column:kid;size:40" json:"kid"`
	Status        string     `gorm:"size:16" json:"status"` // exported | imported | cancelled
	Licenses      int        `json:"licenses"`
	CRLSerial     uint64     `gorm:"column:crl_serial" json:"crl_serial"`
	Request       string     `json:"-"`
	RequestSHA256 string     `gorm:"column:request_sha256;size:64" json:"request_sha256"`
	Signed        string     `json:"-"`
	ExportedBy    string     `gorm:"size:64" json:"exported_by"`
	ExportedAt    time.Time  `json:"exported_at"`
	ImportedBy    string     `gorm:"size:64" json:"imported_by"`
	ImportedAt    *time.Time `json:"imported_at"`
}

// Revocation 是一条吊销；CRLSerial 为 0 表示尚未进入已签名的吊销列表。
type Revocation struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	LicenseID string    `gorm:"uniqueIndex;size:64" json:"license_id"`
	Reason    string    `json:"reason"`
	CRLSerial uint64    `gorm:"column:crl_serial;index" json:"crl_serial"`
	CreatedBy string    `gorm:"size:64" json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// CRLRecord 是一份已签名并发布的吊销列表。
type CRLRecord struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Serial      uint64    `gorm:"uniqueIndex" json:"serial"`
	KID         string    `gorm:"column:kid;size:40" json:"kid"`
	Raw         string    `json:"-"`
	Revoked     int       `json:"revoked"`
	BatchID     uint      `json:"batch_id"`
	PublishedAt time.Time `json:"published_at"`
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

type SystemKV struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string
}

// Models 返回全部模型。
func Models() []any {
	return []any{&User{}, &SigningKey{}, &Customer{}, &Order{}, &License{}, &Batch{}, &Revocation{}, &CRLRecord{}, &AuditLog{}, &SystemKV{}}
}
