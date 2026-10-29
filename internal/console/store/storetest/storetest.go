// Package storetest 为测试提供空数据库：设置 ETOS_TEST_DB（postgres:// DSN）时每个测试使用独立的 schema，
// 否则使用临时 SQLite 文件。CI 两种都跑，保证 SQLite 与 PostgreSQL 行为一致。
package storetest

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
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
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil { // schema 名由本函数生成，非外部输入
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if db, err := admin.DB(); err == nil {
			db.Close()
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
			db.Close()
		}
	})
	return st
}
