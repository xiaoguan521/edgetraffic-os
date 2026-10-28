package apply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 漂移检测（规格书 5.6）：current 目录内容与 MANIFEST 不一致时，TreeDigest 返回 "drifted:..."。
func TestTreeDigestDetectsDrift(t *testing.T) {
	mt, err := os.ReadFile("../../render/defaults/mime.types")
	if err != nil {
		t.Skip("mime.types fixture not found:", err)
	}
	b, err := render.Render(render.Input{Revision: "r000000", MimeTypes: mt})
	if err != nil {
		t.Fatal(err)
	}
	tgz, err := b.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root}}
	if err := a.Bootstrap("r000000", tgz, b.Manifest.Digest); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(root, "releases", "r000000")

	check := func(name string, wantDrift bool) {
		t.Helper()
		d, err := a.TreeDigest()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.HasPrefix(d, "drifted"); got != wantDrift {
			t.Fatalf("%s: drifted=%v, want %v (%s)", name, got, wantDrift, d)
		}
		if !wantDrift && d != b.Manifest.Digest {
			t.Fatalf("%s: digest %s, want %s", name, d, b.Manifest.Digest)
		}
	}
	check("pristine", false)

	f := filepath.Join(cur, "syapp.conf")
	orig, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append(orig, []byte("# manual edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	check("modified file", true)

	if err := os.WriteFile(f, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	check("restored", false)

	extra := filepath.Join(cur, "conf.d", "zz-manual.conf")
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("server {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check("extra file", true)
	_ = os.Remove(extra)

	if err := os.Remove(filepath.Join(cur, "mime.types")); err != nil {
		t.Fatal(err)
	}
	check("deleted file", true)
}

// 授权只读时 agent 用 CurrentFiles 与新版本比较，只放行证书续期（规格书 8.6）。
func TestCurrentFilesForCertOnlyCheck(t *testing.T) {
	mt, err := os.ReadFile("../../render/defaults/mime.types")
	if err != nil {
		t.Skip("mime.types fixture not found:", err)
	}
	b, err := render.Render(render.Input{Revision: "r000000", MimeTypes: mt})
	if err != nil {
		t.Fatal(err)
	}
	tgz, _ := b.TarGz()
	a := &Applier{C: Config{ConfRoot: t.TempDir()}}
	if err := a.Bootstrap("r000000", tgz, b.Manifest.Digest); err != nil {
		t.Fatal(err)
	}
	cur, err := a.CurrentFiles()
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range b.Files {
		if string(cur[n]) != string(want) {
			t.Fatalf("%s differs from the bundle", n)
		}
	}
	if len(cur) != len(b.Files) {
		t.Fatalf("got %d files, bundle has %d", len(cur), len(b.Files))
	}
	next := map[string][]byte{}
	for k, v := range cur {
		next[k] = v
	}
	next["syapp.conf"] = append(append([]byte(nil), next["syapp.conf"]...), '#')
	if render.CertOnlyChange(cur, next) {
		t.Fatal("a config change passed as certificate-only")
	}
}

// 漂移恢复：用同一版本的干净副本原子替换被手改的目录，保留被改动的副本备查。
func TestReplaceDriftedDir(t *testing.T) {
	mt, err := os.ReadFile("../../render/defaults/mime.types")
	if err != nil {
		t.Skip("mime.types fixture not found:", err)
	}
	b, err := render.Render(render.Input{Revision: "r000000", MimeTypes: mt})
	if err != nil {
		t.Fatal(err)
	}
	tgz, _ := b.TarGz()
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root}}
	if err := a.Bootstrap("r000000", tgz, b.Manifest.Digest); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "releases", "r000000", "syapp.conf")
	orig, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(append([]byte(nil), orig...), []byte("# manual\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.TreeDigest(); !strings.HasPrefix(d, "drifted") {
		t.Fatal("expected drift before restore")
	}
	// 摘要不符的包必须被拒绝，且不动现有目录
	if _, err := a.replaceDir("r000000", tgz, "sha256:bogus"); err == nil {
		t.Fatal("bundle with wrong digest accepted")
	}
	backup, err := a.replaceDir("r000000", tgz, b.Manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := a.TreeDigest(); d != b.Manifest.Digest {
		t.Fatalf("still drifted after restore: %s", d)
	}
	kept, err := os.ReadFile(filepath.Join(backup, "syapp.conf"))
	if err != nil || !strings.Contains(string(kept), "# manual") {
		t.Fatalf("drifted copy not kept: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(backup), ".drift-r000000-") {
		t.Fatalf("unexpected backup name %s", backup)
	}
	// 备份目录不会被当成版本（Current / 版本列表只认 r 开头的目录）
	if a.Current() != "r000000" {
		t.Fatalf("current = %s", a.Current())
	}
}
