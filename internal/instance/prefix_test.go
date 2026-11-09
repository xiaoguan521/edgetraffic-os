package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func TestPrefixFirstInstallAndConflict(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("prefix marker ownership test needs root")
	}
	f := newFake(t)
	if err := f.Prefix("/data/syedge"); err != nil {
		t.Fatal(err)
	}
	if got, err := layout.LoadSystemRootAt(f.Root); err != nil || got != "/data/syedge" {
		t.Fatalf("prefix marker: got %q, err %v", got, err)
	}
	if err := f.Prefix("/data/syedge"); err != nil {
		t.Fatalf("same prefix should be idempotent: %v", err)
	}
	if err := f.Prefix("/other"); err == nil || !strings.Contains(err.Error(), "不允许") {
		t.Fatalf("conflicting prefix accepted: %v", err)
	}
}

func TestPrefixRejectsExistingDefaultInstall(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("prefix marker ownership test needs root")
	}
	f := newFake(t)
	def := layout.SystemAt("", "")
	if err := os.MkdirAll(f.path(def.ConfRoot()+"/current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Prefix("/data/syedge"); err == nil || !strings.Contains(err.Error(), "已有默认安装") {
		t.Fatalf("existing default install accepted: %v", err)
	}
}
