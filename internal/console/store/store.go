// Package store 是控制台的数据层（规格书 10.1，MVP 子集）。
//
// MVP 阶段在本包内集中封装全部 GORM 查询（均为参数化查询，禁止拼接 SQL）；
// 正式版按 CLAUDE.md 迁移到 gorm/gen 生成的类型安全查询。
package store

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store 包装数据库连接。
type Store struct{ DB *gorm.DB }

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

// OpenDB 只建立连接，不执行迁移（测试升级路径时使用）。
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
		sqlDB.SetMaxOpenConns(1) // SQLite 单写者
	}
	return db, nil
}

// ErrNotFound 统一表示记录不存在。
var ErrNotFound = errors.New("not found")

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// GetKV / SetKV 读写系统键值。
func (s *Store) GetKV(key string) (string, error) {
	var kv SystemKV
	if err := s.DB.Where("key = ?", key).First(&kv).Error; err != nil {
		return "", notFound(err)
	}
	return kv.Value, nil
}

func (s *Store) SetKV(key, value string) error {
	return s.DB.Save(&SystemKV{Key: key, Value: value}).Error
}

// First 按主键取一条记录。
func First[T any](s *Store, id any) (*T, error) {
	var v T
	if err := s.DB.First(&v, id).Error; err != nil {
		return nil, notFound(err)
	}
	return &v, nil
}
