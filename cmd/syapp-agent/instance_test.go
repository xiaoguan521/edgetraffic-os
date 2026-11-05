package main

import (
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
	if p, lay, err := loadInstance(""); err != nil || p != (render.Profile{}) || lay.Prefix() != "/opt/syapp" {
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
	main := string(b.Files["syapp.conf"])
	if !strings.Contains(main, "/run/syapp-blue/syapp.pid") || strings.Contains(main, "/run/syapp/syapp.pid") {
		t.Fatalf("baseline not using the instance profile:\n%s", main)
	}
}
