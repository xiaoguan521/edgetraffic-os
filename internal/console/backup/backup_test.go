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

func seed(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	db := st.DB
	db.Create(&store.User{Username: "admin", PasswordHash: "h", Role: store.RoleAdmin})
	db.Create(&store.NodeGroup{Name: "default", CurrentRevisionID: "r000002"})
	db.Create(&store.Node{GroupID: 1, Name: "edge-1", Status: "online", AppliedRevision: "r000002", Drifted: true})
	db.Create(&store.Site{GroupID: 1, Name: "shop", Spec: `{"domains":["shop.example.com"]}`, Enabled: true, Version: 3})
	db.Create(&store.Revision{ID: "r000001", GroupID: 1, Bundle: []byte{0x1f, 0x8b, 0, 1, 2, 255}, Digest: "sha256:a"})
	db.Create(&store.Revision{ID: "r000002", GroupID: 1, ParentID: "r000001", Bundle: bytes.Repeat([]byte{7}, 5000), Digest: "sha256:b"})
	dep := &store.Deployment{GroupID: 1, RevisionID: "r000002", Kind: "publish", Status: "APPLIED", StartedAt: time.Now()}
	dep.Nodes = []store.DeploymentNode{{NodeID: 1, NodeName: "edge-1", Status: "APPLIED"}}
	db.Create(dep)
	db.Create(&store.Certificate{Name: "shop", Domains: `["shop.example.com"]`, CertPEM: "CERT", KeyEnc: "v1:sealed", NotAfter: time.Now().Add(time.Hour), Source: "acme", AutoRenew: true})
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
	if dst.DB.Dialector.Name() != "postgres" {
		t.Fatalf("destination is %s", dst.DB.Dialector.Name())
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
