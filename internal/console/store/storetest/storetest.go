// Package storetest 为测试提供独立的数据库。
//
// 默认每个测试一个临时 SQLite 文件；设置 ETOS_TEST_DB=postgres://... 时，每个测试在该库中
// 创建独立的 schema，测试结束后删除。CI 的 postgres 任务用它让控制台测试在 PostgreSQL 上再跑一遍。
package storetest

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

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// Open 返回一个已完成迁移的空库。
func Open(t testing.TB) *store.Store {
	t.Helper()
	dsn := os.Getenv("ETOS_TEST_DB")
	if dsn == "" {
		st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if !strings.HasPrefix(dsn, "postgres") {
		t.Fatalf("ETOS_TEST_DB must be a postgres:// DSN")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
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
	st, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db, err := st.DB.DB(); err == nil {
			_ = db.Close()
		}
	})
	return st
}
