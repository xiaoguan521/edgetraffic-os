package store

import (
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// 数据库迁移（gormigrate）。规则：
//   - 已发布的迁移永不修改：每条迁移使用自己内部定义的表结构快照，不引用会继续演进的模型类型；
//   - 修改 model.go 时在末尾追加一条新迁移（ID 用 年月日序号_说明），只做增量变更；
//   - TestModelsMatchMigrations 检查“全部迁移后的表结构”与“当前模型”一致，
//     TestUpgradeFromEveryMigration 检查从任何一个历史版本升级后与全新安装一致（SQLite 与 PostgreSQL）。

func migrations() []*gormigrate.Migration {
	return []*gormigrate.Migration{
		{ID: "202610270001_init", Migrate: migrateInit},
		{ID: "202610280001_certificates", Migrate: migrateCertificates},
		{ID: "202610290001_acme", Migrate: migrateAcme},
	}
}

// Migrate 把数据库升级到最新版本。
func Migrate(db *gorm.DB) error {
	return gormigrate.New(db, gormigrate.DefaultOptions, migrations()).Migrate()
}

// MigrateTo 只升级到指定迁移（测试升级路径使用）。
func MigrateTo(db *gorm.DB, id string) error {
	return gormigrate.New(db, gormigrate.DefaultOptions, migrations()).MigrateTo(id)
}

// MigrationIDs 按顺序返回全部迁移 ID。
func MigrationIDs() []string {
	var ids []string
	for _, m := range migrations() {
		ids = append(ids, m.ID)
	}
	return ids
}

// ---- 202610270001_init：MVP 的全部表（快照）----

func migrateInit(tx *gorm.DB) error {
	type User struct {
		ID           uint   `gorm:"primaryKey"`
		Username     string `gorm:"uniqueIndex;size:64"`
		PasswordHash string
		Role         string `gorm:"size:16"`
		CreatedAt    time.Time
		LastLoginAt  *time.Time
	}
	type NodeGroup struct {
		ID                uint   `gorm:"primaryKey"`
		Name              string `gorm:"uniqueIndex;size:64"`
		Description       string
		AllowedCIDRs      string `gorm:"column:allowed_cidrs"`
		RealIPFrom        string
		CurrentRevisionID string `gorm:"size:16"`
		CreatedAt         time.Time
		UpdatedAt         time.Time
	}
	type Node struct {
		ID              uint `gorm:"primaryKey"`
		GroupID         uint `gorm:"index"`
		Name            string
		Hostname        string
		RemoteAddr      string
		CertSerial      string
		Status          string `gorm:"size:16"`
		AgentVersion    string
		SyappVersion    string
		SyappSHA256     string
		AppliedRevision string `gorm:"size:16"`
		TreeHash        string
		Drifted         bool
		CPUPercent      float64
		MemUsedBytes    uint64
		ActiveConns     uint64
		RequestsTotal   uint64
		LastSeenAt      *time.Time
		CreatedAt       time.Time
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
		ID        uint   `gorm:"primaryKey"`
		GroupID   uint   `gorm:"uniqueIndex:idx_site_group_name"`
		Name      string `gorm:"uniqueIndex:idx_site_group_name;size:64"`
		Spec      string
		Enabled   bool
		Version   int
		UpdatedBy string
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type Upstream struct {
		ID        uint   `gorm:"primaryKey"`
		GroupID   uint   `gorm:"uniqueIndex:idx_up_group_name"`
		Name      string `gorm:"uniqueIndex:idx_up_group_name;size:64"`
		Spec      string
		UpdatedBy string
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type Revision struct {
		Seq       uint   `gorm:"primaryKey"`
		ID        string `gorm:"uniqueIndex;size:16"`
		GroupID   uint   `gorm:"index"`
		ParentID  string `gorm:"size:16"`
		Digest    string
		Bundle    []byte
		Manifest  string
		Note      string
		CreatedBy string
		CreatedAt time.Time
	}
	type Deployment struct {
		ID         uint   `gorm:"primaryKey"`
		GroupID    uint   `gorm:"index"`
		RevisionID string `gorm:"size:16"`
		Kind       string `gorm:"size:16"`
		Status     string `gorm:"size:16"`
		Note       string
		CreatedBy  string
		StartedAt  time.Time
		FinishedAt *time.Time
	}
	type DeploymentNode struct {
		ID           uint `gorm:"primaryKey"`
		DeploymentID uint `gorm:"index"`
		NodeID       uint
		NodeName     string
		Status       string `gorm:"size:16"`
		Message      string
		Stderr       string
		FinishedAt   *time.Time
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
		ID         uint      `gorm:"primaryKey"`
		TS         time.Time `gorm:"index"`
		Username   string
		IP         string
		Action     string
		ObjectType string
		ObjectID   string
		Detail     string
		Result     string
		PrevHash   string
		Hash       string
	}
	type LicenseStore struct {
		ID         uint `gorm:"primaryKey"`
		Raw        string
		ImportedBy string
		ImportedAt time.Time
	}
	type SystemKV struct {
		Key   string `gorm:"primaryKey;size:64"`
		Value string
	}
	return tx.AutoMigrate(&User{}, &NodeGroup{}, &Node{}, &EnrollToken{}, &Site{}, &Upstream{},
		&Revision{}, &Deployment{}, &DeploymentNode{}, &MetricMinute{}, &AuditLog{}, &LicenseStore{}, &SystemKV{})
}

// ---- 202610280001_certificates：证书库（P6-1）----

func migrateCertificates(tx *gorm.DB) error {
	type Certificate struct {
		ID          uint   `gorm:"primaryKey"`
		Name        string `gorm:"uniqueIndex;size:64"`
		Domains     string
		Subject     string
		Issuer      string
		Serial      string
		NotBefore   time.Time
		NotAfter    time.Time `gorm:"index"`
		Fingerprint string    `gorm:"size:64"`
		CertPEM     string
		KeyEnc      string
		Source      string `gorm:"size:16"`
		UpdatedBy   string
		CreatedAt   time.Time
		UpdatedAt   time.Time
	}
	return tx.AutoMigrate(&Certificate{})
}

// ---- 202610290001_acme：ACME 自动签发（P6-4）----

func migrateAcme(tx *gorm.DB) error {
	type Certificate struct {
		AutoRenew bool
	}
	type AcmeOrder struct {
		ID         uint   `gorm:"primaryKey"`
		Name       string `gorm:"size:64"`
		CertID     uint   `gorm:"index"`
		Domains    string
		Status     string `gorm:"size:16"`
		Step       string
		Error      string
		CreatedBy  string
		CreatedAt  time.Time
		FinishedAt *time.Time
	}
	type AcmeChallenge struct {
		ID        uint   `gorm:"primaryKey"`
		OrderID   uint   `gorm:"index"`
		Domain    string `gorm:"size:255"`
		Token     string `gorm:"uniqueIndex;size:128"`
		KeyAuth   string `gorm:"size:255"`
		CreatedAt time.Time
	}
	// 早期安装可能已经有这一列（当时的迁移直接用了演进中的模型），先检查再加
	if !tx.Migrator().HasColumn(&Certificate{}, "AutoRenew") {
		if err := tx.Migrator().AddColumn(&Certificate{}, "AutoRenew"); err != nil {
			return err
		}
	}
	return tx.AutoMigrate(&AcmeOrder{}, &AcmeChallenge{})
}
