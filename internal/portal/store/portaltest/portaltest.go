// Package portaltest 为授权后台的测试提供独立的数据库：默认临时 SQLite 文件；
// 设置 ETOS_TEST_DB=postgres://... 时在该库中为每个测试创建独立 schema，测试结束后删除。
package portaltest

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
)

// Open 返回一个已完成迁移的空库。
func Open(t testing.TB) *store.Store {
	t.Helper()
	db := OpenRaw(t)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return &store.Store{DB: db}
}

// OpenRaw 返回一个未执行迁移的空库。
func OpenRaw(t testing.TB) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("ETOS_TEST_DB")
	if dsn == "" {
		db, err := store.OpenDB(filepath.Join(t.TempDir(), "portal.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if s, err := db.DB(); err == nil {
				_ = s.Close()
			}
		})
		return db
	}
	if !strings.HasPrefix(dsn, "postgres") {
		t.Fatalf("ETOS_TEST_DB must be a postgres:// DSN")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "p_" + hex.EncodeToString(b) // 本函数生成，非外部输入
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if db, err := admin.DB(); err == nil {
			_ = db.Close()
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.OpenDB(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
	})
	return db
}
