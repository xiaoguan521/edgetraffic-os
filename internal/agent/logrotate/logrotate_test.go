package logrotate

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.json")
	errlog := filepath.Join(dir, "error.log")
	empty := filepath.Join(dir, "stream.log")
	_ = os.WriteFile(access, []byte(`{"a":1}`+"\n"), 0o644)
	_ = os.WriteFile(errlog, []byte("boom\n"), 0o644)
	_ = os.WriteFile(empty, nil, 0o644)
	reopened := 0
	now := time.Date(2026, 11, 5, 0, 5, 0, 0, time.UTC)
	r := &Rotator{Files: []string{access, errlog, empty, filepath.Join(dir, "missing.log")}, Keep: 2, MaxBytes: 1 << 20,
		Reopen: func() error {
			reopened++
			_ = os.WriteFile(access, nil, 0o644)
			_ = os.WriteFile(errlog, nil, 0o644)
			return nil
		},
		Now: func() time.Time { return now }}

	// 首次检查只记下日期，不轮转（不知道文件属于哪一天）
	if n, err := r.Check(); err != nil || n != 0 || reopened != 0 {
		t.Fatalf("first check: %d %v reopen=%d", n, err, reopened)
	}
	// 跨天：轮转非空文件、压缩、通知数据面重新打开一次
	now = now.Add(24 * time.Hour)
	n, err := r.Check()
	if err != nil || n != 2 || reopened != 1 {
		t.Fatalf("daily rotate: %d %v reopen=%d", n, err, reopened)
	}
	gz := filepath.Join(dir, "access.json.20261106-000500.gz")
	f, err := os.Open(gz)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := gzip.NewReader(f)
	b, _ := io.ReadAll(zr)
	_ = f.Close()
	if string(b) != `{"a":1}`+"\n" {
		t.Fatalf("rotated content = %q", b)
	}
	if st, _ := os.Stat(empty); st == nil {
		t.Fatal("empty file must stay in place")
	}
	// 同一天内再检查：不轮转
	if n, _ := r.Check(); n != 0 {
		t.Fatal("rotated twice on the same day")
	}
	// 超过大小上限：当天也轮转
	r.MaxBytes = 4
	_ = os.WriteFile(errlog, []byte("0123456789"), 0o644)
	now = now.Add(time.Minute)
	if n, _ := r.Check(); n != 1 {
		t.Fatalf("size rotate: %d", n)
	}
	// 只保留 Keep 份
	for i := 0; i < 3; i++ {
		_ = os.WriteFile(errlog, []byte("0123456789"), 0o644)
		now = now.Add(time.Minute)
		_, _ = r.Check()
	}
	var kept []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "error.log.") {
			kept = append(kept, e.Name())
		}
	}
	sort.Strings(kept)
	if len(kept) != 2 {
		t.Fatalf("kept %v, want 2", kept)
	}
}

func TestReopenFailureKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "error.log")
	_ = os.WriteFile(f, []byte("x\n"), 0o644)
	now := time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC)
	r := &Rotator{Files: []string{f}, Keep: 3, MaxBytes: 1, Reopen: func() error { return os.ErrPermission }, Now: func() time.Time { return now }}
	if _, err := r.Check(); err == nil {
		t.Fatal("reopen failure not reported")
	}
	// 重命名后的文件没有被压缩删除：数据面仍在写它，不能丢
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".gz") {
			t.Fatal("compressed a file the data plane may still be writing")
		}
	}
}
