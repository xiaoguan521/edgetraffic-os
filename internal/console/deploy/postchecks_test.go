package deploy

import (
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func siteRevision(t *testing.T) *store.Revision {
	t.Helper()
	b := &render.Bundle{Files: map[string][]byte{
		"conf.d/site-a.conf": []byte("server {\n    listen 80;\n    server_name a.example.com;\n}\n"),
		"conf.d/site-w.conf": []byte("server {\n    server_name *.w.example.com;\n}\n"),
	}}
	data, err := b.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	return &store.Revision{ID: "r000001", Bundle: data}
}

// 读取冻结版本失败必须报错，不能返回空列表（空列表表示“本来没有探活项”，会让业务探活被悄悄省略）。
func TestPostChecksReportsUnreadableRevision(t *testing.T) {
	d := &Service{}
	checks, err := d.postChecks(&store.Revision{ID: "r000001", Bundle: []byte("not a tar.gz")})
	if err == nil {
		t.Fatalf("unreadable revision must return an error, got checks=%v", checks)
	}
	if checks != nil {
		t.Fatalf("no checks may be returned together with an error: %v", checks)
	}
}

func TestPostChecksListsSites(t *testing.T) {
	d := &Service{}
	checks, err := d.postChecks(siteRevision(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0].Host != "a.example.com" || checks[1].Host != "probe.w.example.com" {
		t.Fatalf("unexpected checks: %v", checks)
	}
}

// 没有站点文件是合法的“无探活项”，与读取失败要能区分。
func TestPostChecksEmptyRevisionIsNotAnError(t *testing.T) {
	d := &Service{}
	data, err := (&render.Bundle{Files: map[string][]byte{"syedge.conf": []byte("events {}")}}).TarGz()
	if err != nil {
		t.Fatal(err)
	}
	checks, err := d.postChecks(&store.Revision{ID: "r000001", Bundle: data})
	if err != nil || len(checks) != 0 {
		t.Fatalf("checks=%v err=%v", checks, err)
	}
}
