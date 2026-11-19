package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestParseInstance(t *testing.T) {
	p, err := parseInstance([]byte(`{"instance":"blue","http_port":18080,"https_port":18443,"status_port":8082}`), "blue")
	if err != nil || p.HTTP() != 18080 || p.Status() != 8082 {
		t.Fatalf("valid instance: %+v %v", p, err)
	}
	for body, want := range map[string]string{
		`{"instance":"green"}`: "not \"blue\"",
		`{"instance":"blue","http_port":8080,"https_port":8080}`:                                                        "both",
		`{"instance":"blue","mode":"user","root":"/home/x/edge","http_port":8080,"https_port":8443,"status_port":8082}`: "user-mode",
		`{`: "unexpected end",
	} {
		if _, err := parseInstance([]byte(body), "blue"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", body, want, err)
		}
	}
	if p, lay, err := loadInstance(""); err != nil || p != (render.Profile{}) || lay.Prefix() != "/opt/syedge" {
		t.Fatalf("default instance: %+v %v", p, err)
	}
	if _, _, err := loadInstance("Bad"); err == nil {
		t.Fatal("invalid instance name accepted")
	}
}

// 具名实例的初始版本按实例档案生成（路径与端口不落在默认实例上）。
func TestBaselineForInstance(t *testing.T) {
	b, err := render.BaselineFor(render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082})
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	if !strings.Contains(main, "/opt/syedge/instances/blue/run/syedge.pid") || strings.Contains(main, "/opt/syedge/run/syedge.pid") {
		t.Fatalf("baseline not using the instance profile:\n%s", main)
	}
}

// user 模式：默认实例与具名实例都从安装根目录下的档案读取；根目录必须一致、模式必须是 user。
func TestLoadInstanceUserMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "edge")
	write := func(dir, body string) {
		t.Helper()
		_ = os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := loadInstanceAt(root, ""); err == nil {
		t.Fatal("user mode without instance.json accepted")
	}
	write(root, `{"mode":"user","root":"`+root+`","http_port":8080,"https_port":8443,"status_port":8082}`)
	p, lay, err := loadInstanceAt(root, "")
	if err != nil || p.HTTP() != 8080 || lay.PidFile() != root+"/run/syedge.pid" || lay.WrapperBin() != root+"/bin/syedge-wrapper" {
		t.Fatalf("user default: %+v %s %v", p, lay.PidFile(), err)
	}
	write(filepath.Join(root, "instances", "blue"), `{"instance":"blue","mode":"user","root":"/elsewhere","http_port":18080,"https_port":18443,"status_port":8083}`)
	if _, _, err := loadInstanceAt(root, "blue"); err == nil || !strings.Contains(err.Error(), "install root") {
		t.Fatalf("root mismatch: %v", err)
	}
	write(filepath.Join(root, "instances", "red"), `{"instance":"red","http_port":18080,"https_port":18443,"status_port":8083}`)
	if _, _, err := loadInstanceAt(root, "red"); err == nil || !strings.Contains(err.Error(), "system-mode") {
		t.Fatalf("system instance under a user root: %v", err)
	}
}

func TestLoadIndependentInstanceLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "edge")
	dir := filepath.Join(root, "instances", "blue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"instance":"blue","mode":"user","root":"` + root + `","resource_mode":"independent","http_port":18080,"https_port":18443,"status_port":8083}`
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, lay, err := loadInstanceAt(root, "blue")
	if err != nil || p.ResourceMode != "independent" || lay.Bin() != filepath.Join(dir, "sbin/syedge") || lay.Modules() != filepath.Join(dir, "modules") {
		t.Fatalf("profile/layout = %+v %+v %v", p, lay, err)
	}
}
