package deploy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range files {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte(b))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestStaticBundle(t *testing.T) {
	d, g := newTestService(t)
	// dist/ 顶层目录会被去掉；隐藏文件不发布
	sb, err := d.ImportStatic(g.ID, "web-v1", zipOf(t, map[string]string{"dist/index.html": "<h1>v1</h1>", "dist/js/app.js": "1", "dist/.DS_Store": "x"}), "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Files != 2 {
		t.Fatalf("files = %d", sb.Files)
	}
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", `..\etc\passwd`, `C:\evil.conf`} {
		if _, err := d.ImportStatic(g.ID, "evil", zipOf(t, map[string]string{"index.html": "ok", name: "x"}), "alice", ""); err == nil {
			t.Fatalf("bad path accepted: %q", name)
		}
	}
	if _, err := d.ImportStatic(g.ID, "no-index", zipOf(t, map[string]string{"dist/js/app.js": "1"}), "alice", ""); err == nil {
		t.Fatal("bundle without root index.html accepted")
	}
	spec, _ := json.Marshal(render.Site{Name: "web", Domains: []string{"web.example.com"},
		Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "static", Static: &render.StaticSpec{BundleID: sb.ID}}}})
	_ = query.Use(d.Store.DB).Site.WithContext(context.Background()).Create(&store.Site{GroupID: g.ID, Name: "web", Spec: string(spec), Enabled: true})
	files, _, err := d.Preview(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/js/app.js") {
			found = true
		}
	}
	if !found {
		t.Fatalf("bundle files not in the release: %+v", files)
	}
}

// 隐藏内容跳过、根目录 .well-known/ 保留、入口页可选（extractStatic 的纯逻辑）。
func TestExtractStaticHiddenAndIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		opt   StaticImportOptions
		want  []string // nil 表示应当报错
	}{
		{"hidden dirs skipped, not rejected", map[string]string{
			"dist/index.html": "x", "dist/.git/config": "x", "dist/.git/objects/ab/cd": "x", "dist/js/.cache/a": "x", "dist/.env": "x"},
			StaticImportOptions{}, []string{"index.html"}},
		{"root junk does not block dist/ strip", map[string]string{
			"dist/index.html": "x", ".DS_Store": "x", "__MACOSX/dist/._index.html": "x"},
			StaticImportOptions{}, []string{"index.html"}},
		{"well-known kept at root", map[string]string{
			"index.html": "x", ".well-known/security.txt": "x", ".well-known/acme-challenge/t": "x"},
			StaticImportOptions{}, []string{".well-known/security.txt", "index.html"}},
		{"well-known kept after dist/ strip", map[string]string{
			"dist/index.html": "x", "dist/.well-known/security.txt": "x"},
			StaticImportOptions{}, []string{".well-known/security.txt", "index.html"}},
		{"nested well-known skipped", map[string]string{
			"index.html": "x", "a/.well-known/x": "x"},
			StaticImportOptions{}, []string{"index.html"}},
		{"no index rejected by default", map[string]string{"files/a.pdf": "x"}, StaticImportOptions{}, nil},
		{"no index allowed when asked", map[string]string{"files/a.pdf": "x", "files/b.png": "x"},
			StaticImportOptions{AllowNoIndex: true}, []string{"a.pdf", "b.png"}},
		{"only hidden content", map[string]string{".git/config": "x"}, StaticImportOptions{AllowNoIndex: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := extractStatic(zipOf(t, tc.files), tc.opt)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("accepted: %v", files)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(files))
			for n := range files {
				got = append(got, n)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("files = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := extractStatic(zipOf(t, map[string]string{"js/a.js": "x"}), StaticImportOptions{}); !IsStaticNoIndex(err) {
		t.Fatalf("missing index must be reported as errStaticNoIndex: %v", err)
	}
}
