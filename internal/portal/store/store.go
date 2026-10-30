package store

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store 包装数据库连接。
type Store struct{ DB *gorm.DB }

// ErrNotFound 统一表示记录不存在。
var ErrNotFound = errors.New("not found")

// Open 打开数据库并升级到最新版本：dsn 以 postgres:// 开头时使用 PostgreSQL，否则视为 SQLite 文件路径。
func Open(dsn string) (*Store, error) {
	db, err := OpenDB(dsn)
	if err != nil {
		return nil, err
	}
	if err := Migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// OpenDB 只建立连接，不执行迁移。
func OpenDB(dsn string) (*gorm.DB, error) {
	var dial gorm.Dialector
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		dial = postgres.Open(dsn)
	} else {
		dial = sqlite.Open(dsn + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	}
	db, err := gorm.Open(dial, &gorm.Config{Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
		SlowThreshold: time.Second, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true})})
	if err != nil {
		return nil, err
	}
	if sqlDB, err := db.DB(); err == nil && !strings.HasPrefix(dsn, "postgres") {
		sqlDB.SetMaxOpenConns(1)
	}
	return db, nil
}

func migrations() []*gormigrate.Migration {
	return []*gormigrate.Migration{
		{ID: "202610300001_portal_init", Migrate: migrateInit},
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
