package store

import (
	"time"

	"gorm.io/gorm"
)

// 迁移规则同控制台（internal/console/store/migrations.go）：已发布的迁移永不修改，每条迁移使用自己的表结构快照。

// ---- 202610300001_portal_init：授权后台的全部表（快照）----

func migrateInit(tx *gorm.DB) error {
	type User struct {
		ID           uint   `gorm:"primaryKey"`
		Username     string `gorm:"uniqueIndex;size:64"`
		DisplayName  string `gorm:"size:64"`
		PasswordHash string
		Roles        string `gorm:"size:64"`
		Disabled     bool
		CreatedAt    time.Time
		LastLoginAt  *time.Time
	}
	type SigningKey struct {
		ID          uint   `gorm:"primaryKey"`
		KID         string `gorm:"column:kid;uniqueIndex;size:40"`
		PublicKey   string `gorm:"size:64"`
		Fingerprint string `gorm:"size:80"`
		Status      string `gorm:"size:16"`
		Note        string
		CreatedBy   string `gorm:"size:64"`
		CreatedAt   time.Time
		UpdatedAt   time.Time
	}
	type Customer struct {
		ID         uint   `gorm:"primaryKey"`
		Code       string `gorm:"uniqueIndex;size:32"`
		Name       string `gorm:"size:128"`
		Contact    string `gorm:"size:64"`
		Email      string `gorm:"size:128"`
		Phone      string `gorm:"size:32"`
		SalesOwner string `gorm:"size:64"`
		Note       string
		CreatedBy  string `gorm:"size:64"`
		CreatedAt  time.Time
		UpdatedAt  time.Time
	}
	type Order struct {
		ID         uint   `gorm:"primaryKey"`
		OrderNo    string `gorm:"uniqueIndex;size:32"`
		CustomerID uint   `gorm:"index"`
		Edition    string `gorm:"size:16"`
		Nodes      int
		Sites      int
		Days       int
		GraceDays  int
		Amount     string `gorm:"size:32"`
		ContractNo string `gorm:"size:64"`
		Note       string
		CreatedBy  string `gorm:"size:64"`
		CreatedAt  time.Time
	}
	type License struct {
		ID              uint   `gorm:"primaryKey"`
		LicenseID       string `gorm:"uniqueIndex;size:64"`
		CustomerID      uint   `gorm:"index"`
		OrderID         uint   `gorm:"index"`
		ParentLicenseID string `gorm:"size:64"`
		Status          string `gorm:"index;size:16"`
		Payload         string
		InstallationID  string    `gorm:"index;size:64"`
		Binding         string    `gorm:"size:24"`
		ExpiresAt       time.Time `gorm:"index"`
		Raw             string
		BatchID         uint   `gorm:"index"`
		SubmittedBy     string `gorm:"size:64"`
		ApprovedBy      string `gorm:"size:64"`
		RejectReason    string
		IssuedAt        *time.Time
		RevokedAt       *time.Time
		RevokeReason    string
		CreatedBy       string `gorm:"size:64"`
		CreatedAt       time.Time
		UpdatedAt       time.Time
	}
	type Batch struct {
		ID            uint   `gorm:"primaryKey"`
		BatchNo       string `gorm:"uniqueIndex;size:16"`
		KID           string `gorm:"column:kid;size:40"`
		Status        string `gorm:"size:16"`
		Licenses      int
		CRLSerial     uint64 `gorm:"column:crl_serial"`
		Request       string
		RequestSHA256 string `gorm:"column:request_sha256;size:64"`
		Signed        string
		ExportedBy    string `gorm:"size:64"`
		ExportedAt    time.Time
		ImportedBy    string `gorm:"size:64"`
		ImportedAt    *time.Time
	}
	type Revocation struct {
		ID        uint   `gorm:"primaryKey"`
		LicenseID string `gorm:"uniqueIndex;size:64"`
		Reason    string
		CRLSerial uint64 `gorm:"column:crl_serial;index"`
		CreatedBy string `gorm:"size:64"`
		CreatedAt time.Time
	}
	type CRLRecord struct {
		ID          uint   `gorm:"primaryKey"`
		Serial      uint64 `gorm:"uniqueIndex"`
		KID         string `gorm:"column:kid;size:40"`
		Raw         string
		Revoked     int
		BatchID     uint
		PublishedAt time.Time
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
	type SystemKV struct {
		Key   string `gorm:"primaryKey;size:64"`
		Value string
	}
	return tx.AutoMigrate(&User{}, &SigningKey{}, &Customer{}, &Order{}, &License{}, &Batch{}, &Revocation{}, &CRLRecord{}, &AuditLog{}, &SystemKV{})
}
