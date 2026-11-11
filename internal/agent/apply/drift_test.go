package apply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestDriftReport(t *testing.T) {
	root := t.TempDir()
	checkDriftReport(t, root)
}

func TestLegacyDriftOmitsEveryFileContent(t *testing.T) {
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root}}
	rel := filepath.Join(root, "releases", "r000001")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	b := render.Repack("r000001", map[string][]byte{"syedge.conf": []byte("old secret"), configbundle.LegacyMarker: []byte("{}")})
	for name, data := range b.Files {
		if err := os.WriteFile(filepath.Join(rel, name), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(rel, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "syedge.conf"), []byte("new secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "password.txt"), []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	r, err := a.Drift()
	if err != nil || r == nil || len(r.Files) != 2 {
		t.Fatalf("drift = %+v, %v", r, err)
	}
	for _, f := range r.Files {
		if !f.Omitted || len(f.Content) != 0 {
			t.Fatalf("legacy drift disclosed content: %+v", f)
		}
	}
}

func TestDriftReportWithSymlinkedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "managed")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	checkDriftReport(t, root)
}

func checkDriftReport(t *testing.T, root string) {
	t.Helper()
	a := &Applier{C: Config{ConfRoot: root}}
	rel := filepath.Join(root, "releases", "r000001")
	_ = os.MkdirAll(filepath.Join(rel, "conf.d"), 0o755)
	_ = os.MkdirAll(filepath.Join(rel, "certs"), 0o755)
	in := page("r000001", "hello")
	if len(in.MimeTypes) == 0 {
		in.MimeTypes = []byte("types {}\n")
	}
	tgz, _ := bundleOf(t, in)
	files := untar(t, tgz)
	for n, b := range files {
		p := filepath.Join(rel, n)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, b, 0o644)
	}
	if err := os.Symlink(rel, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if r, err := a.Drift(); err != nil || r != nil {
		t.Fatalf("clean tree: %v %+v", err, r)
	}
	// 手改站点配置、增加一个私钥文件、删除 mime.types
	site := filepath.Join(rel, "conf.d/site-it.conf")
	b, _ := os.ReadFile(site)
	_ = os.WriteFile(site, []byte(strings.Replace(string(b), "server {", "server {\n    client_max_body_size  50m;", 1)), 0o644)
	_ = os.WriteFile(filepath.Join(rel, "certs/evil.key"), []byte("-----BEGIN PRIVATE KEY-----"), 0o600)
	_ = os.Remove(filepath.Join(rel, "mime.types"))
	r, err := a.Drift()
	if err != nil || r == nil || r.Revision != "r000001" || r.TreeHash == "" {
		t.Fatalf("drift: %v %+v", err, r)
	}
	by := map[string]DriftFile{}
	for _, f := range r.Files {
		by[f.Path] = f
	}
	if f := by["conf.d/site-it.conf"]; f.Change != "modified" || !strings.Contains(string(f.Content), "client_max_body_size") {
		t.Fatalf("modified site: %+v", f)
	}
	if f := by["certs/evil.key"]; f.Change != "added" || !f.Omitted || len(f.Content) != 0 || f.SHA256 == "" {
		t.Fatalf("private key must be reported by hash only: %+v", f)
	}
	if f := by["mime.types"]; f.Change != "removed" {
		t.Fatalf("removed: %+v", f)
	}
	// 同一状态的指纹稳定
	r2, _ := a.Drift()
	if r2.TreeHash != r.TreeHash {
		t.Fatal("tree hash not stable")
	}
	// current 指向受管目录之外时不读取
	_ = os.Remove(filepath.Join(root, "current"))
	_ = os.Symlink(t.TempDir(), filepath.Join(root, "current"))
	if r, err := a.Drift(); err != nil || r != nil {
		t.Fatalf("outside releases: %v %+v", err, r)
	}
}

func untar(t *testing.T, tgz []byte) map[string][]byte {
	t.Helper()
	m, err := render.ReadTarGz(tgz)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
