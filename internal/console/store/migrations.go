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
		{ID: "202610300001_release_requests", Migrate: migrateReleaseRequests},
		{ID: "202610300002_snippets", Migrate: migrateSnippets},
		{ID: "202610300003_config_imports", Migrate: migrateConfigImports},
		{ID: "202610300004_api_tokens", Migrate: migrateAPITokens},
		{ID: "202610300005_alerts", Migrate: migrateAlerts},
		{ID: "202610300006_node_drifts", Migrate: migrateNodeDrifts},
		{ID: "202610300007_dataplane_upgrades", Migrate: migrateDataplaneUpgrades},
		{ID: "202610300008_account_security", Migrate: migrateAccountSecurity},
		{ID: "202610300009_streams", Migrate: migrateStreams},
		{ID: "202610300010_canary", Migrate: migrateCanary},
		{ID: "202610300011_static_bundles", Migrate: migrateStaticBundles},
		{ID: "202610310001_gm_tls", Migrate: migrateGMTLS},
		{ID: "202610310002_rules_lua", Migrate: migrateRulesLua},
		{ID: "202611050001_instance_profile", Migrate: migrateInstanceProfile},
		{ID: "202611050002_running_version", Migrate: migrateRunningVersion},
		{ID: "202611180001_token_version", Migrate: migrateTokenVersion},
		{ID: "202611280001_node_disk_usage", Migrate: migrateNodeDiskUsage},
		{ID: "202611280002_node_vip_ha", Migrate: migrateNodeVIPHA},
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

// ---- 202610300001_release_requests：发布审批（P7）----

func migrateReleaseRequests(tx *gorm.DB) error {
	type ReleaseRequest struct {
		ID             uint   `gorm:"primaryKey"`
		GroupID        uint   `gorm:"index"`
		RevisionID     string `gorm:"size:16"`
		BaseRevisionID string `gorm:"size:16"`
		Digest         string
		Status         string `gorm:"index;size:16"`
		Note           string
		RequestedBy    string `gorm:"size:64"`
		RequestedAt    time.Time
		ReviewedBy     string `gorm:"size:64"`
		ReviewedAt     *time.Time
		ReviewComment  string
		DeploymentID   uint
	}
	return tx.AutoMigrate(&ReleaseRequest{})
}

// ---- 202610300002_snippets：自定义片段（P7）----

func migrateSnippets(tx *gorm.DB) error {
	type Snippet struct {
		ID            uint   `gorm:"primaryKey"`
		GroupID       uint   `gorm:"uniqueIndex:idx_snippet_slot"`
		SiteID        uint   `gorm:"uniqueIndex:idx_snippet_slot"`
		Scope         string `gorm:"uniqueIndex:idx_snippet_slot;size:16"`
		RoutePath     string `gorm:"uniqueIndex:idx_snippet_slot;size:255"`
		Name          string `gorm:"uniqueIndex:idx_snippet_slot;size:32"`
		Content       string
		ContentSHA256 string `gorm:"column:content_sha256;size:64"`
		Pending       string
		PendingDelete bool
		PendingBy     string `gorm:"size:64"`
		PendingAt     *time.Time
		PendingNote   string
		Origin        string `gorm:"size:32"`
		ApprovedBy    string `gorm:"size:64"`
		ApprovedAt    *time.Time
		CreatedBy     string `gorm:"size:64"`
		CreatedAt     time.Time
		UpdatedAt     time.Time
	}
	return tx.AutoMigrate(&Snippet{})
}

// ---- 202610300003_config_imports：配置导入（P7）----

func migrateConfigImports(tx *gorm.DB) error {
	type ConfigImport struct {
		ID           uint   `gorm:"primaryKey"`
		GroupID      uint   `gorm:"index"`
		Source       string `gorm:"size:16"`
		FileName     string
		Report       string
		ReportSHA256 string `gorm:"column:report_sha256;size:64"`
		Sites        int
		Full         int
		Partial      int
		Failed       int
		Issues       int
		CreatedBy    string `gorm:"size:64"`
		CreatedAt    time.Time
		AppliedBy    string `gorm:"size:64"`
		AppliedAt    *time.Time
		Result       string
	}
	return tx.AutoMigrate(&ConfigImport{})
}

// ---- 202610300004_api_tokens：开放 API 令牌（P9）----

func migrateAPITokens(tx *gorm.DB) error {
	type APIToken struct {
		ID         uint   `gorm:"primaryKey"`
		Name       string `gorm:"size:64"`
		Username   string `gorm:"index;size:64"`
		Role       string `gorm:"size:16"`
		TokenHash  string `gorm:"uniqueIndex;size:64"`
		Prefix     string `gorm:"size:16"`
		ExpiresAt  *time.Time
		LastUsedAt *time.Time
		LastUsedIP string `gorm:"size:64"`
		Revoked    bool
		CreatedBy  string `gorm:"size:64"`
		CreatedAt  time.Time
	}
	return tx.AutoMigrate(&APIToken{})
}

// ---- 202610300005_alerts：告警通道、规则与事件（P9）----

func migrateAlerts(tx *gorm.DB) error {
	type AlertChannel struct {
		ID        uint   `gorm:"primaryKey"`
		Name      string `gorm:"uniqueIndex;size:64"`
		Type      string `gorm:"size:16"`
		URL       string
		SecretEnc string
		SMTPUser  string `gorm:"column:smtp_user"`
		MailFrom  string
		MailTo    string
		Enabled   bool
		CreatedBy string `gorm:"size:64"`
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type AlertRule struct {
		ID         uint   `gorm:"primaryKey"`
		Kind       string `gorm:"uniqueIndex;size:32"`
		Enabled    bool
		Threshold  float64
		ChannelIDs string
		UpdatedBy  string `gorm:"size:64"`
		UpdatedAt  time.Time
	}
	type AlertEvent struct {
		ID         uint   `gorm:"primaryKey"`
		Kind       string `gorm:"size:32"`
		ObjectKey  string `gorm:"index;size:128"`
		Message    string
		Status     string `gorm:"index;size:16"`
		FiredAt    time.Time
		ResolvedAt *time.Time
		Notify     string
	}
	return tx.AutoMigrate(&AlertChannel{}, &AlertRule{}, &AlertEvent{})
}

// ---- 202610300006_node_drifts：漂移差异（P7）----

func migrateNodeDrifts(tx *gorm.DB) error {
	type NodeDrift struct {
		NodeID     uint   `gorm:"primaryKey"`
		RevisionID string `gorm:"size:16"`
		TreeHash   string `gorm:"size:80"`
		Files      string
		ReportedAt time.Time
	}
	return tx.AutoMigrate(&NodeDrift{})
}

// ---- 202610300007_dataplane_upgrades：数据面版本库与分批热升级（P8）----

func migrateDataplaneUpgrades(tx *gorm.DB) error {
	type Node struct {
		Arch string `gorm:"size:16"`
	}
	type DataplanePackage struct {
		ID         uint   `gorm:"primaryKey"`
		Name       string `gorm:"uniqueIndex:idx_pkg;size:32"`
		Version    string `gorm:"uniqueIndex:idx_pkg;size:32"`
		Arch       string `gorm:"uniqueIndex:idx_pkg;size:16"`
		Variant    string `gorm:"uniqueIndex:idx_pkg;size:16"`
		SHA256     string `gorm:"column:sha256;size:64"`
		Size       int64
		KID        string `gorm:"column:kid;size:40"`
		Sig        string
		Data       []byte
		UploadedBy string `gorm:"size:64"`
		CreatedAt  time.Time
	}
	type DataplaneUpgrade struct {
		ID         uint   `gorm:"primaryKey"`
		GroupID    uint   `gorm:"index"`
		Version    string `gorm:"size:32"`
		Variant    string `gorm:"size:16"`
		Status     string `gorm:"size:16"`
		Nodes      string
		Note       string
		CreatedBy  string `gorm:"size:64"`
		CreatedAt  time.Time
		FinishedAt *time.Time
	}
	if !tx.Migrator().HasColumn(&Node{}, "Arch") {
		if err := tx.Migrator().AddColumn(&Node{}, "Arch"); err != nil {
			return err
		}
	}
	return tx.AutoMigrate(&DataplanePackage{}, &DataplaneUpgrade{})
}

// ---- 202610300008_account_security：账号安全策略与外部身份（P9）----

func migrateAccountSecurity(tx *gorm.DB) error {
	type User struct {
		Source             string `gorm:"size:16"`
		Disabled           bool
		MustChangePassword bool
		PasswordChangedAt  *time.Time
		PasswordHistory    string
		FailedLogins       int
		LockedUntil        *time.Time
	}
	for _, col := range []string{"Source", "Disabled", "MustChangePassword", "PasswordChangedAt", "PasswordHistory", "FailedLogins", "LockedUntil"} {
		if !tx.Migrator().HasColumn(&User{}, col) {
			if err := tx.Migrator().AddColumn(&User{}, col); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 202610300009_streams：四层代理（P9）----

func migrateStreams(tx *gorm.DB) error {
	type StreamService struct {
		ID        uint   `gorm:"primaryKey"`
		GroupID   uint   `gorm:"uniqueIndex:idx_stream_group_name"`
		Name      string `gorm:"uniqueIndex:idx_stream_group_name;size:64"`
		Spec      string
		Enabled   bool
		UpdatedBy string `gorm:"size:64"`
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	return tx.AutoMigrate(&StreamService{})
}

// ---- 202610300010_canary：灰度发布（P9）----

func migrateCanary(tx *gorm.DB) error {
	type CanaryRelease struct {
		ID             uint   `gorm:"primaryKey"`
		GroupID        uint   `gorm:"index"`
		RevisionID     string `gorm:"size:16"`
		BaseRevisionID string `gorm:"size:16"`
		Status         string `gorm:"size:16"`
		Options        string
		Batches        string
		Note           string
		Message        string
		CreatedBy      string `gorm:"size:64"`
		CreatedAt      time.Time
		FinishedAt     *time.Time
	}
	return tx.AutoMigrate(&CanaryRelease{})
}

// ---- 202610300011_static_bundles：静态资源发布（P9）----

func migrateStaticBundles(tx *gorm.DB) error {
	type StaticBundle struct {
		ID         uint   `gorm:"primaryKey"`
		GroupID    uint   `gorm:"index"`
		Name       string `gorm:"size:128"`
		SHA256     string `gorm:"column:sha256;size:64"`
		Size       int64
		Files      int
		Data       []byte
		UploadedBy string `gorm:"size:64"`
		CreatedAt  time.Time
	}
	return tx.AutoMigrate(&StaticBundle{})
}

// ---- 202610310001_gm_tls：国密 TLCP 双证书（P8 国密变体）----

func migrateGMTLS(tx *gorm.DB) error {
	type Certificate struct {
		Kind           string `gorm:"size:8"`
		EncCertPEM     string
		EncKeyEnc      string
		EncFingerprint string `gorm:"size:64"`
	}
	type Node struct {
		TLSLib string `gorm:"size:16"`
	}
	if err := tx.AutoMigrate(&Certificate{}); err != nil {
		return err
	}
	return tx.AutoMigrate(&Node{})
}

// ---- 202610310002_rules_lua：规则积木的 IP 集 / 地域库与 syedge-lua 附加包上报（P10）----

func migrateRulesLua(tx *gorm.DB) error {
	type IPSet struct {
		ID        uint   `gorm:"primaryKey"`
		GroupID   uint   `gorm:"uniqueIndex:idx_ipset_group_name"`
		Name      string `gorm:"uniqueIndex:idx_ipset_group_name;size:64"`
		Label     string `gorm:"size:128"`
		Source    string `gorm:"size:32"`
		CIDRs     string `gorm:"column:cidrs"`
		Count     int
		UpdatedBy string `gorm:"size:64"`
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type Node struct {
		LuaAddon string `gorm:"size:40"`
	}
	if err := tx.AutoMigrate(&IPSet{}); err != nil {
		return err
	}
	return tx.AutoMigrate(&Node{})
}

// ---- 202611050001_instance_profile：节点组实例档案与节点主机标识（设计 0001，P11-B）----

func migrateInstanceProfile(tx *gorm.DB) error {
	type NodeGroup struct {
		InstanceProfile string
	}
	type Node struct {
		HostKey string `gorm:"index;size:64"`
	}
	if err := tx.AutoMigrate(&NodeGroup{}); err != nil {
		return err
	}
	return tx.AutoMigrate(&Node{})
}

// ---- 202611050002_running_version：实例实际运行的数据面版本（设计 0001 第 5.6 节）----

func migrateRunningVersion(tx *gorm.DB) error {
	type Node struct {
		RunningVersion string `gorm:"size:32"`
	}
	return tx.AutoMigrate(&Node{})
}

// ---- 202611180001_token_version：用户会话版本，退出、改口令、重置口令时递增，使已签发的会话立即失效 ----

func migrateTokenVersion(tx *gorm.DB) error {
	type User struct {
		TokenVersion int `gorm:"not null;default:0"`
	}
	return tx.AutoMigrate(&User{})
}

// ---- 202611280001_node_disk_usage：边缘节点磁盘与 inode 使用率上报 ----

func migrateNodeDiskUsage(tx *gorm.DB) error {
	type Node struct {
		DiskUsedPercent  float64
		DiskInodePercent float64
	}
	return tx.AutoMigrate(&Node{})
}

// ---- 202611280002_node_vip_ha：节点组入口高可用 VIP 与节点网卡 IP 上报 ----

func migrateNodeVIPHA(tx *gorm.DB) error {
	type NodeGroup struct {
		VIPs string `gorm:"column:vips"`
	}
	type Node struct {
		LocalIPs string `gorm:"column:local_ips"`
	}
	if err := tx.AutoMigrate(&NodeGroup{}); err != nil {
		return err
	}
	return tx.AutoMigrate(&Node{})
}
