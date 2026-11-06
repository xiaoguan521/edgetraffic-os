package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func baselineNode(t *testing.T) *apply.Applier {
	t.Helper()
	root := t.TempDir()
	ap := &apply.Applier{C: apply.Config{ConfRoot: root}}
	b, err := render.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	tgz, _ := b.TarGz()
	if err := ap.Bootstrap(render.BaselineRevision, tgz, b.Manifest.Digest); err != nil {
		t.Fatal(err)
	}
	return ap
}

func TestLocalConfigCheck(t *testing.T) {
	now := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)
	// 全新节点：没有 current
	if p, err := localConfigCheck(&apply.Applier{C: apply.Config{ConfRoot: t.TempDir()}}, false, t.TempDir(), now); err != nil || p != "" {
		t.Fatalf("fresh node: %q %v", p, err)
	}
	// 未改动的初始版本：允许注册，不备份
	ap := baselineNode(t)
	bk := t.TempDir()
	if p, err := localConfigCheck(ap, false, bk, now); err != nil || p != "" {
		t.Fatalf("pristine baseline: %q %v", p, err)
	}
	// 独立模式下加了站点：默认拒绝
	site := filepath.Join(ap.C.ConfRoot, "current", "conf.d", "www.conf")
	if err := os.MkdirAll(filepath.Dir(site), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(site, []byte("server { listen 80; server_name www.example.com; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localConfigCheck(ap, false, bk, now); err == nil || !strings.Contains(err.Error(), "-replace-local-config") {
		t.Fatalf("customised node accepted: %v", err)
	}
	if ents, _ := os.ReadDir(bk); len(ents) != 0 {
		t.Fatal("backup written while refusing")
	}
	// 确认接管：先备份
	p, err := localConfigCheck(ap, true, filepath.Join(bk, "backups"), now)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup %s: %v %v", p, fi, err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		found[h.Name] = true
	}
	if !found["conf.d/www.conf"] || !found["syedge.conf"] {
		t.Fatalf("backup content: %v", found)
	}
	// 配置本身不动：在控制台首次发布前继续使用本机配置
	if _, err := os.Stat(site); err != nil {
		t.Fatal("local config removed")
	}
}
