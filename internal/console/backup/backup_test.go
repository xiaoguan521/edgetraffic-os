package backup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// 本包测试关注备份内容与一致性，用轻量的口令派生参数加快运行；生产参数由 TestProductionKDF 覆盖。
func TestMain(m *testing.M) {
	kdfTime, kdfMemoryKiB, kdfThreads = 1, 8*1024, 1
	os.Exit(m.Run())
}

// 生产参数下完整备份、恢复一次；并确认用轻量参数无法解开生产参数的备份（参数确实生效）。
func TestProductionKDF(t *testing.T) {
	saved := [3]uint32{kdfTime, kdfMemoryKiB, kdfThreads}
	kdfTime, kdfMemoryKiB, kdfThreads = 3, 64*1024, 4
	defer func() { kdfTime, kdfMemoryKiB, kdfThreads = saved[0], saved[1], saved[2] }()
	dir := t.TempDir()
	src := storetest.Open(t)
	seed(t, src, dir)
	pass := []byte("production parameters")
	var buf bytes.Buffer
	if _, err := Create(&buf, src, dir, pass, Options{}); err != nil {
		t.Fatal(err)
	}
	kdfTime, kdfMemoryKiB, kdfThreads = 1, 8*1024, 1
	if _, err := Restore(bytes.NewReader(buf.Bytes()), storetest.Open(t), t.TempDir(), pass, false); err == nil {
		t.Fatal("backup made with production parameters opened with different parameters")
	}
	kdfTime, kdfMemoryKiB, kdfThreads = 3, 64*1024, 4
	if _, err := Restore(bytes.NewReader(buf.Bytes()), storetest.Open(t), t.TempDir(), pass, false); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	db := st.DB
	now := time.Now().UTC().Truncate(time.Second)
	db.Create(&store.User{Username: "admin", PasswordHash: "h", Role: store.RoleAdmin})
	db.Create(&store.APIToken{Name: "ci-tok", Username: "admin", Role: store.RoleAdmin, TokenHash: "th_1", Prefix: "etos_1234567"})
	db.Create(&store.LicenseStore{Raw: "LIC-RAW", ImportedBy: "admin", ImportedAt: now})
	db.Create(&store.Certificate{Name: "shop", Domains: `["shop.example.com"]`, CertPEM: "CERT", KeyEnc: "v1:sealed", NotBefore: now, NotAfter: now.Add(time.Hour), Source: "acme", AutoRenew: true})
	db.Create(&store.AcmeOrder{Name: "shop", CertID: 1, Domains: `["shop.example.com"]`, Status: "valid", Step: "完成", CreatedBy: "admin"})
	db.Create(&store.AlertChannel{Name: "ops-hook", Type: "webhook", URL: "https://hooks.example.com/1", SecretEnc: "v1:sec", Enabled: true, CreatedBy: "admin"})
	db.Create(&store.AlertRule{Kind: "node_offline", Enabled: true, Threshold: 5, ChannelIDs: "1", UpdatedBy: "admin"})
	db.Create(&store.AlertEvent{Kind: "node_offline", ObjectKey: "node:1", Message: "offline", Status: "firing", FiredAt: now})
	db.Create(&store.DataplanePackage{Name: "syedge", Version: "1.30.6", Arch: "amd64", Variant: "standard", SHA256: "pkgsha", Size: 4, KID: "k1", Sig: "sig", Data: []byte("DATA"), UploadedBy: "admin"})
	db.Create(&store.NodeGroup{Name: "default", CurrentRevisionID: "r000002"})
	db.Create(&store.Node{GroupID: 1, Name: "edge-1", Status: "online", AppliedRevision: "r000002", Drifted: true})
	db.Create(&store.NodeDrift{NodeID: 1, RevisionID: "r000002", TreeHash: "sha256:drift", Files: `[{"path":"syedge.conf"}]`, ReportedAt: now})
	db.Create(&store.EnrollToken{TokenHash: "enroll_h1", GroupID: 1, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"})
	db.Create(&store.StaticBundle{GroupID: 1, Name: "web-v1", SHA256: "sbsha", Size: 6, Files: 1, Data: []byte("TARGZ!"), UploadedBy: "admin"})
	db.Create(&store.IPSet{GroupID: 1, Name: "office", Label: "Office", Source: "manual", CIDRs: `["10.0.0.0/8"]`, Count: 1, UpdatedBy: "admin"})
	db.Create(&store.Upstream{GroupID: 1, Name: "backend", Spec: `{"name":"backend","servers":[{"addr":"10.0.0.1:8080"}]}`, UpdatedBy: "admin"})
	db.Create(&store.StreamService{GroupID: 1, Name: "tcp-9000", Spec: `{"listen_port":9000}`, Enabled: true, UpdatedBy: "admin"})
	db.Create(&store.Site{GroupID: 1, Name: "shop", Spec: `{"domains":["shop.example.com"]}`, Enabled: true, Version: 3})
	db.Create(&store.Snippet{GroupID: 1, SiteID: 1, Scope: "server", Content: "add_header X-Test 1;", ContentSHA256: "snipsha", Origin: "manual", CreatedBy: "admin", ApprovedBy: "ops"})
	db.Create(&store.ConfigImport{GroupID: 1, Source: "dump", FileName: "syedge.conf", Report: `{}`, ReportSHA256: "impsha", Sites: 1, Full: 1, CreatedBy: "admin"})
	db.Create(&store.Revision{ID: "r000001", GroupID: 1, Bundle: []byte{0x1f, 0x8b, 0, 1, 2, 255}, Digest: "sha256:a"})
	db.Create(&store.Revision{ID: "r000002", GroupID: 1, ParentID: "r000001", Bundle: bytes.Repeat([]byte{7}, 5000), Digest: "sha256:b"})
	dep := &store.Deployment{GroupID: 1, RevisionID: "r000002", Kind: "publish", Status: "APPLIED", StartedAt: now}
	dep.Nodes = []store.DeploymentNode{{NodeID: 1, NodeName: "edge-1", Status: "APPLIED"}}
	db.Create(dep)
	db.Create(&store.ReleaseRequest{GroupID: 1, RevisionID: "r000002", BaseRevisionID: "r000001", Digest: "sha256:b", Status: "approved", RequestedBy: "ops", RequestedAt: now, DeploymentID: dep.ID})
	db.Create(&store.CanaryRelease{GroupID: 1, RevisionID: "r000002", BaseRevisionID: "r000001", Status: "done", Options: `{}`, Batches: `[]`, CreatedBy: "admin"})
	db.Create(&store.DataplaneUpgrade{GroupID: 1, Version: "1.30.6", Variant: "standard", Status: "done", Nodes: `[]`, CreatedBy: "admin"})
	db.Create(&store.MetricMinute{NodeID: 1, Site: "shop", Minute: 1, Requests: 9})
	_ = st.SetKV("command_key", "secret-ed25519")
	al := &audit.Logger{DB: db}
	for i := 0; i < 3; i++ {
		_ = al.Write(audit.Entry{Username: "admin", Action: "site.update", ObjectType: "site", ObjectID: "shop"})
	}
	_ = os.MkdirAll(filepath.Join(dir, "pki"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "pki", "ca.key"), []byte("CA-KEY"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "pki", "ca.crt"), []byte("CA-CRT"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret.key"), bytes.Repeat([]byte{9}, 32), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "installation.json"), []byte(`{"installation_id":"inst_abc"}`), 0o600)
}

// 守卫测试：store.Models() 中除明确排除的临时表外，每一张持久化表都必须纳入 backup.tables()，
// 且表名与 GORM 解析的模型表名完全一致，防止新增模型漏入备份（BKP-07）。
func TestTablesCoverAllStoreModels(t *testing.T) {
	st := storetest.Open(t)
	excluded := map[string]bool{
		"acme_challenges": true, // 进行中的 HTTP-01 临时挑战，不随备份持久化
	}
	want := map[string]bool{}
	for _, m := range store.Models() {
		stmt := &gorm.Statement{DB: st.DB}
		if err := stmt.Parse(m); err != nil {
			t.Fatalf("parse model %T: %v", m, err)
		}
		name := stmt.Schema.Table
		if excluded[name] {
			continue
		}
		want[name] = true
	}
	got := map[string]bool{}
	for _, tb := range tables() {
		stmt := &gorm.Statement{DB: st.DB}
		if err := stmt.Parse(tb.model); err != nil {
			t.Fatalf("parse table %s: %v", tb.name, err)
		}
		if stmt.Schema.Table != tb.name {
			t.Fatalf("table name mismatch: registered %q, schema %q", tb.name, stmt.Schema.Table)
		}
		if got[tb.name] {
			t.Fatalf("duplicate table %q in backup list", tb.name)
		}
		if !want[tb.name] {
			t.Fatalf("table %q in backup list is not a persistent store.Models() entry", tb.name)
		}
		got[tb.name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("persistent model table %q is missing from backup.tables()", name)
		}
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src := storetest.Open(t)
	seed(t, src, srcDir)
	pass := []byte("correct horse battery staple")

	var buf bytes.Buffer
	sum, err := Create(&buf, src, srcDir, pass, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("CA-KEY")) || bytes.Contains(buf.Bytes(), []byte("secret-ed25519")) {
		t.Fatal("backup is not encrypted")
	}
	if sum.Tables["revisions"] != 2 || sum.Tables["audit_logs"] != 3 || sum.Tables["metric_minutes"] != 0 {
		t.Fatalf("summary: %+v", sum.Tables)
	}

	if _, err := Restore(bytes.NewReader(buf.Bytes()), storetest.Open(t), t.TempDir(), []byte("wrong"), false); err == nil || !strings.Contains(err.Error(), "口令") {
		t.Fatalf("wrong passphrase accepted: %v", err)
	}
	bad := append([]byte(nil), buf.Bytes()...)
	bad[len(bad)-5] ^= 1
	if _, err := Restore(bytes.NewReader(bad), storetest.Open(t), t.TempDir(), pass, false); err == nil {
		t.Fatal("tampered backup accepted")
	}

	dst := storetest.Open(t)
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst, dstDir, pass, false); err != nil {
		t.Fatal(err)
	}
	var revs []store.Revision
	dst.DB.Order("seq").Find(&revs)
	if len(revs) != 2 || revs[0].ID != "r000001" || !bytes.Equal(revs[0].Bundle, []byte{0x1f, 0x8b, 0, 1, 2, 255}) || len(revs[1].Bundle) != 5000 {
		t.Fatalf("revisions not restored byte for byte: %+v", revs)
	}
	var node store.Node
	dst.DB.First(&node)
	if !node.Drifted || node.AppliedRevision != "r000002" {
		t.Fatalf("node fields lost: %+v", node)
	}
	var c store.Certificate
	dst.DB.First(&c)
	if c.KeyEnc != "v1:sealed" || !c.AutoRenew || c.Source != "acme" {
		t.Fatalf("certificate fields lost: %+v", c)
	}
	var dn int64
	dst.DB.Model(&store.DeploymentNode{}).Count(&dn)
	if dn != 1 {
		t.Fatalf("deployment nodes: %d", dn)
	}
	var sb store.StaticBundle
	if err := dst.DB.First(&sb).Error; err != nil || sb.Name != "web-v1" || !bytes.Equal(sb.Data, []byte("TARGZ!")) {
		t.Fatalf("static bundle lost: %v %+v", err, sb)
	}
	var pkg store.DataplanePackage
	if err := dst.DB.First(&pkg).Error; err != nil || !bytes.Equal(pkg.Data, []byte("DATA")) {
		t.Fatalf("dataplane package lost: %v %+v", err, pkg)
	}
	for _, tb := range tables() {
		if tb.name == "metric_minutes" {
			continue
		}
		var n int64
		if err := dst.DB.Model(tb.model).Count(&n).Error; err != nil || n == 0 {
			t.Fatalf("table %s count after restore = %d (%v)", tb.name, n, err)
		}
	}
	if v, _ := dst.GetKV("command_key"); v != "secret-ed25519" {
		t.Fatal("system kv not restored")
	}
	if bad, err := (&audit.Logger{DB: dst.DB}).Verify(); err != nil || bad != 0 {
		t.Fatalf("audit chain broken after restore at %d: %v", bad, err)
	}
	for _, f := range []string{"pki/ca.key", "secret.key", "installation.json"} {
		a, _ := os.ReadFile(filepath.Join(srcDir, f))
		b, err := os.ReadFile(filepath.Join(dstDir, f))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s not restored: %v", f, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dstDir, "pki", "ca.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode %v", fi.Mode().Perm())
	}
	// 新写入的记录不与恢复的主键冲突（PostgreSQL 序列已重置）
	if err := dst.DB.Create(&store.User{Username: "ops", PasswordHash: "h", Role: store.RoleViewer}).Error; err != nil {
		t.Fatalf("insert after restore: %v", err)
	}
	if err := (&audit.Logger{DB: dst.DB}).Write(audit.Entry{Username: "ops", Action: "x"}); err != nil {
		t.Fatal(err)
	}
	if bad, err := (&audit.Logger{DB: dst.DB}).Verify(); err != nil || bad != 0 {
		t.Fatal("audit chain broken after a new entry")
	}

	// 目标库非空时拒绝，避免覆盖正在使用的控制台
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst, t.TempDir(), pass, false); err == nil || !strings.Contains(err.Error(), "不是空库") {
		t.Fatalf("restore into a non-empty database accepted: %v", err)
	}
}

func TestBackupIncludesMetricsWhenAsked(t *testing.T) {
	dir := t.TempDir()
	src := storetest.Open(t)
	seed(t, src, dir)
	var buf bytes.Buffer
	sum, err := Create(&buf, src, dir, []byte("pass phrase 123"), Options{Metrics: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tables["metric_minutes"] != 1 {
		t.Fatalf("metrics not included: %+v", sum.Tables)
	}
}

// SQLite → PostgreSQL：备份恢复也是迁移数据库的方式（需要 ETOS_TEST_DB 指向 PostgreSQL）。
func TestMigrateSQLiteToPostgres(t *testing.T) {
	if os.Getenv("ETOS_TEST_DB") == "" {
		t.Skip("set ETOS_TEST_DB to a PostgreSQL DSN")
	}
	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, src, srcDir)
	var buf bytes.Buffer
	pass := []byte("migrate to postgres")
	if _, err := Create(&buf, src, srcDir, pass, Options{Metrics: true}); err != nil {
		t.Fatal(err)
	}
	dst := storetest.Open(t) // PostgreSQL
	if dst.DB.Name() != "postgres" {
		t.Fatalf("destination is %s", dst.DB.Name())
	}
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst, t.TempDir(), pass, false); err != nil {
		t.Fatal(err)
	}
	var revs int64
	dst.DB.Model(&store.Revision{}).Count(&revs)
	if revs != 2 {
		t.Fatalf("revisions: %d", revs)
	}
	if bad, err := (&audit.Logger{DB: dst.DB}).Verify(); err != nil || bad != 0 {
		t.Fatalf("audit chain broken after migration: %d %v", bad, err)
	}
	if err := dst.DB.Create(&store.Revision{ID: "r000003", GroupID: 1}).Error; err != nil {
		t.Fatalf("new revision after migration: %v", err)
	}
}

// 备份在一个事务里读取所有表：并发写入不会让备份出现半个发布（版本有了、发布记录还没有）。
func TestBackupIsConsistentSnapshot(t *testing.T) {
	dir := t.TempDir()
	st := storetest.Open(t)
	seed(t, st, dir)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 3; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = st.DB.Transaction(func(tx *gorm.DB) error {
				id := fmt.Sprintf("r%06d", i)
				if err := tx.Create(&store.Revision{ID: id, GroupID: 1}).Error; err != nil {
					return err
				}
				return tx.Create(&store.Deployment{GroupID: 1, RevisionID: id, Kind: "publish", Status: "APPLIED", StartedAt: time.Now()}).Error
			})
		}
	}()
	pass := []byte("snapshot pass")
	for k := 0; k < 5; k++ {
		var buf bytes.Buffer
		if _, err := Create(&buf, st, dir, pass, Options{}); err != nil {
			t.Fatal(err)
		}
		dst := storetest.Open(t)
		if _, err := Restore(bytes.NewReader(buf.Bytes()), dst, t.TempDir(), pass, false); err != nil {
			t.Fatal(err)
		}
		var revs, deps int64
		dst.DB.Model(&store.Revision{}).Count(&revs)
		dst.DB.Model(&store.Deployment{}).Count(&deps)
		// 种子数据：2 个版本、1 次发布；之后每个版本恰好对应一次发布
		if revs-2 != deps-1 {
			close(stop)
			<-done
			t.Fatalf("inconsistent snapshot: %d revisions vs %d deployments", revs, deps)
		}
	}
	close(stop)
	<-done
}

// 控制台启动过但还没初始化（只有启动时生成的 system_kvs）时允许恢复，以备份中的密钥为准。
func TestRestoreIntoStartedButEmptyConsole(t *testing.T) {
	dir := t.TempDir()
	src := storetest.Open(t)
	seed(t, src, dir)
	var buf bytes.Buffer
	pass := []byte("pass phrase 123")
	if _, err := Create(&buf, src, dir, pass, Options{}); err != nil {
		t.Fatal(err)
	}
	dst := storetest.Open(t)
	_ = dst.SetKV("command_key", "generated-at-startup")
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst, t.TempDir(), pass, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := dst.GetKV("command_key"); v != "secret-ed25519" {
		t.Fatalf("command_key = %q", v)
	}
}

func restoreFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	src := storetest.Open(t)
	seed(t, src, dir)
	var buf bytes.Buffer
	pass := []byte("pass phrase 123")
	if _, err := Create(&buf, src, dir, pass, Options{}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), pass
}

// 密钥文件写不进去时，恢复必须整体失败且不动数据库：否则库已有数据、重试被“不是空库”拒绝，只能手工清库。
func TestRestoreFileWriteFailureLeavesDatabaseUntouchedAndRetryable(t *testing.T) {
	data, pass := restoreFixture(t)
	dataDir := t.TempDir()
	// pki 是普通文件：写 pki/ca.crt 时 MkdirAll 必然失败
	if err := os.WriteFile(filepath.Join(dataDir, "pki"), []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := storetest.Open(t)
	if _, err := Restore(bytes.NewReader(data), dst, dataDir, pass, false); err == nil {
		t.Fatal("restore succeeded although a key file could not be written")
	}
	var users, revs int64
	dst.DB.Model(&store.User{}).Count(&users)
	dst.DB.Model(&store.Revision{}).Count(&revs)
	if users != 0 || revs != 0 {
		t.Fatalf("database was modified by a failed restore: users=%d revisions=%d", users, revs)
	}
	for _, f := range []string{"installation.json", "secret.key"} {
		if _, err := os.Stat(filepath.Join(dataDir, f)); err == nil {
			t.Fatalf("%s was left behind by a failed restore", f)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dataDir, ".restore-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	// 排除故障后直接重试即可成功
	if err := os.Remove(filepath.Join(dataDir, "pki")); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bytes.NewReader(data), dst, dataDir, pass, false); err != nil {
		t.Fatalf("retry after fixing the cause failed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca.key")); err != nil || len(b) == 0 {
		t.Fatalf("ca.key not restored on retry: %v", err)
	}
}

// 数据库导入失败时也不能留下任何密钥文件。
func TestRestoreDatabaseFailureLeavesNoFiles(t *testing.T) {
	data, pass := restoreFixture(t)
	dataDir := t.TempDir()
	dst := storetest.Open(t)
	// 让导入失败：预先放入一个会与备份主键冲突的行（system_kvs 以外的表必须为空，这里改用关闭库连接来模拟写入失败）
	sqlDB, err := dst.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	if _, err := Restore(bytes.NewReader(data), dst, dataDir, pass, false); err == nil {
		t.Fatal("restore succeeded on a closed database")
	}
	entries, _ := os.ReadDir(dataDir)
	if len(entries) != 0 {
		t.Fatalf("files left behind after a database failure: %v", entries)
	}
}
