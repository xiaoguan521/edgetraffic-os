package deploy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
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
	if _, err := d.ImportStatic(g.ID, "evil", zipOf(t, map[string]string{"../../etc/passwd": "x", "a;b": "y"}), "alice", ""); err == nil {
		t.Fatal("bad paths accepted")
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
