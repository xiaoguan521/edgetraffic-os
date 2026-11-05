package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

func TestBackupRestoreCLI(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	st, err := store.Open(filepath.Join(src, "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.DB.Create(&store.NodeGroup{Name: "default"})
	if err := os.MkdirAll(filepath.Join(src, "pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pki", "ca.key"), []byte("K"), 0o600); err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pf, []byte("long enough passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.etosbk")
	var so, se bytes.Buffer

	if rc := backupMain([]string{"backup", "-data", src, "-out", out}, &so, &se); rc == 0 || !strings.Contains(se.String(), "ETOS_BACKUP_PASSPHRASE") {
		t.Fatalf("backup without passphrase: rc=%d %s", rc, se.String())
	}
	short := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	se.Reset()
	if rc := backupMain([]string{"backup", "-data", src, "-out", out, "-passphrase-file", short}, &so, &se); rc == 0 {
		t.Fatal("short passphrase accepted")
	}
	se.Reset()
	if rc := backupMain([]string{"backup", "-data", src, "-out", out, "-passphrase-file", pf}, &so, &se); rc != 0 {
		t.Fatalf("backup: %s", se.String())
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", fi, err)
	}
	if _, err := os.Stat(out + ".partial"); err == nil {
		t.Fatal("partial file left behind")
	}
	// verify-backup 与 restore -dry-run 不修改目标目录
	so.Reset()
	se.Reset()
	if rc := backupMain([]string{"verify-backup", "-in", out, "-passphrase-file", pf}, &so, &se); rc != 0 || !strings.Contains(so.String(), "备份校验通过") {
		t.Fatalf("verify-backup: rc=%d out=%s err=%s", rc, so.String(), se.String())
	}
	so.Reset()
	se.Reset()
	if rc := backupMain([]string{"restore", "-data", dst, "-in", out, "-passphrase-file", pf, "-dry-run"}, &so, &se); rc != 0 || !strings.Contains(so.String(), "dry-run") {
		t.Fatalf("restore -dry-run: rc=%d out=%s err=%s", rc, so.String(), se.String())
	}
	if _, err := os.Stat(filepath.Join(dst, "console.db")); err == nil {
		t.Fatal("dry-run should not create database in destination")
	}

	if rc := backupMain([]string{"restore", "-data", dst, "-in", out, "-passphrase-file", pf}, &so, &se); rc != 0 {
		t.Fatalf("restore: %s", se.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "pki", "ca.key")); string(b) != "K" {
		t.Fatal("ca.key not restored")
	}
	rst, _ := store.Open(filepath.Join(dst, "console.db"))
	var n int64
	rst.DB.Model(&store.AuditLog{}).Where("action = ?", "system.restore").Count(&n)
	if n != 1 {
		t.Fatal("restore not audited")
	}
	// 再次恢复到同一目标：非空库，拒绝
	se.Reset()
	if rc := backupMain([]string{"restore", "-data", dst, "-in", out, "-passphrase-file", pf, "-force"}, &so, &se); rc == 0 || !strings.Contains(se.String(), "不是空库") {
		t.Fatalf("second restore: rc=%d %s", rc, se.String())
	}

	// backup-prune 保留最新 N 份并清理过期备份
	pruneDir := t.TempDir()
	now := time.Now()
	for i, ageDays := range []int{1, 5, 40, 50} {
		p := filepath.Join(pruneDir, filepath.Base(out)+strings.Repeat("x", i)+".etosbk")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-time.Duration(ageDays) * 24 * time.Hour)
		_ = os.Chtimes(p, mt, mt)
	}
	so.Reset()
	se.Reset()
	if rc := backupMain([]string{"backup-prune", "-dir", pruneDir, "-keep-last", "2", "-keep-days", "30"}, &so, &se); rc != 0 {
		t.Fatalf("backup-prune: rc=%d %s", rc, se.String())
	}
	des, _ := os.ReadDir(pruneDir)
	if len(des) != 2 {
		t.Fatalf("expected 2 backups remaining after prune, got %d", len(des))
	}
}
