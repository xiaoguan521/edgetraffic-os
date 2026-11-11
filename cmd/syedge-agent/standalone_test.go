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

func TestLocalConfigBackupsRemainDistinctWithinOneSecond(t *testing.T) {
	ap := baselineNode(t)
	config := filepath.Join(ap.C.ConfRoot, "current", "syedge.conf")
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# changed\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	bk := t.TempDir()
	now := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)
	first, err := localConfigCheck(ap, true, bk, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := localConfigCheck(ap, true, bk, now)
	if err != nil || first == second {
		t.Fatalf("backup collision: first=%q second=%q err=%v", first, second, err)
	}
	after, err := os.ReadFile(first)
	if err != nil || string(after) != string(before) {
		t.Fatalf("previous backup was removed or changed: %v", err)
	}
}

func TestTarDirPreservesExistingArchive(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "existing.tar.gz")
	if err := os.WriteFile(out, []byte("previous backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tarDir(root, out); err == nil {
		t.Fatal("existing backup was accepted")
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "previous backup" {
		t.Fatalf("previous backup changed: %q, %v", data, err)
	}
}

func TestRepairPackageBaselinePreservesStandaloneConfiguration(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "pristine", true: "customized"}[custom], func(t *testing.T) {
			root := t.TempDir()
			defaults := filepath.Join(root, "defaults")
			conf := filepath.Join(root, "conf")
			current := filepath.Join(conf, "releases", render.BaselineRevision)
			b, err := render.Baseline()
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{defaults, current} {
				if err := b.WriteDir(dir); err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(dir, "syedge.conf")
				original, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, append(original, []byte("# custom-root package rewrite\n")...), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// 软件包升级可能在 defaults 中留下旧文件；它们不属于当前软件包清单。
			if err := os.WriteFile(filepath.Join(defaults, "obsolete.conf"), []byte("# old package\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("releases", render.BaselineRevision), filepath.Join(conf, "current")); err != nil {
				t.Fatal(err)
			}
			site := filepath.Join(current, "conf.d", "www.conf")
			if custom {
				if err := os.MkdirAll(filepath.Dir(site), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(site, []byte("# local site\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := repairPackageBaseline(conf, defaults); err != nil {
				t.Fatal(err)
			}
			ap := &apply.Applier{C: apply.Config{ConfRoot: conf}}
			_, err = localConfigCheck(ap, false, t.TempDir(), time.Now())
			if custom {
				if err == nil || !strings.Contains(err.Error(), "-replace-local-config") {
					t.Fatalf("modified config must still be detected: %v", err)
				}
				if got, err := os.ReadFile(site); err != nil || string(got) != "# local site\n" {
					t.Fatalf("local site lost: %q %v", got, err)
				}
			} else if err != nil {
				t.Fatalf("pristine package baseline treated as modified: %v", err)
			}
		})
	}
}

func TestRepairPackageBaselineLeavesNonPackageManifestAlone(t *testing.T) {
	root := t.TempDir()
	defaults := filepath.Join(root, "defaults")
	conf := filepath.Join(root, "conf")
	current := filepath.Join(conf, "releases", render.BaselineRevision)
	b, err := render.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{defaults, current} {
		if err := b.WriteDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("releases", render.BaselineRevision), filepath.Join(conf, "current")); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(current, render.ManifestName)
	want := []byte("administrator-managed manifest\n")
	if err := os.WriteFile(manifest, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repairPackageBaseline(conf, defaults); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(manifest); err != nil || string(got) != string(want) {
		t.Fatalf("non-package manifest changed: %q %v", got, err)
	}
}

func TestRepairPackageBaselineRejectsSymlinkedReleases(t *testing.T) {
	root := t.TempDir()
	defaults := filepath.Join(root, "defaults")
	conf := filepath.Join(root, "conf")
	outside := filepath.Join(root, "outside")
	b, err := render.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteDir(defaults); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteDir(filepath.Join(outside, render.BaselineRevision)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(conf, "releases")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", render.BaselineRevision), filepath.Join(conf, "current")); err != nil {
		t.Fatal(err)
	}
	if err := repairPackageBaseline(conf, defaults); err == nil {
		t.Fatal("symlinked release directory was accepted")
	}
}
