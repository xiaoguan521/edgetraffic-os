package wrapper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// user 模式（设计 0001 第 6.2 节）：同一套白名单，路径来自安装根目录，命令只带 -p <实例前缀>（reload 等不带 -c、-e）。
func TestBuildUserMode(t *testing.T) {
	root := "/home/ops/edge"
	for _, c := range []struct {
		argv []string
		path string
		args []string
	}{
		{[]string{"test", "r000123"}, root + "/sbin/syedge", []string{"-t", "-q", "-p", root, "-c", root + "/conf/releases/r000123/syedge.conf"}},
		{[]string{"reload"}, root + "/sbin/syedge", []string{"-p", root, "-s", "reload"}},
		{[]string{"--instance", "blue", "quit"}, root + "/sbin/syedge", []string{"-p", root + "/instances/blue", "-s", "quit"}},
	} {
		cmd, err := BuildAt(root, c.argv)
		if err != nil {
			t.Fatalf("%q: %v", c.argv, err)
		}
		if cmd.Path != c.path || strings.Join(cmd.Args, " ") != strings.Join(c.args, " ") || cmd.Root != root {
			t.Errorf("%q: %s %q (root %q)", c.argv, cmd.Path, cmd.Args, cmd.Root)
		}
	}
	// 与 system 模式相同的注入拒绝
	for _, argv := range [][]string{{"test", "../x"}, {"reload", "-c"}, {"--instance", "Bad", "reload"}, {"restart"}} {
		if _, err := BuildAt(root, argv); !errors.Is(err, ErrUsage) {
			t.Errorf("%q accepted in user mode", argv)
		}
	}
	// system 模式不变
	if cmd, _ := Build([]string{"reload"}); strings.Join(cmd.Args, " ") != "-p /opt/syedge -s reload" || cmd.Root != "" {
		t.Errorf("system reload = %q", cmd.Args)
	}
	p, err := DefaultPathsAt(root, "blue")
	if err != nil || p.Bin != root+"/sbin/syedge" || p.PidFile != root+"/instances/blue/run/syedge.pid" {
		t.Fatalf("user upgrade paths = %+v %v", p, err)
	}
}

func TestIndependentRuntimeUsesTrustedInstanceProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "edge")
	dir := filepath.Join(root, "instances", "blue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, InstanceFile)
	body := `{"instance":"blue","mode":"user","root":"` + root + `","resource_mode":"independent"}`
	if err := os.WriteFile(profile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err := BuildAt(root, []string{"--instance", "blue", "reload"})
	if err != nil || cmd.Path != filepath.Join(dir, "sbin/syedge") {
		t.Fatalf("command = %+v %v", cmd, err)
	}
	paths, err := DefaultPathsAt(root, "blue")
	if err != nil || paths.Modules != filepath.Join(dir, "modules") || paths.Versions != filepath.Join(dir, "versions") {
		t.Fatalf("upgrade paths = %+v %v", paths, err)
	}
	if err := os.Chmod(profile, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildAt(root, []string{"--instance", "blue", "reload"}); err == nil {
		t.Fatal("writable profile was trusted")
	}
}

func TestRootFromExecutable(t *testing.T) {
	if r, err := RootFromExecutable("/usr/libexec/syedge/syedge-wrapper"); err != nil || r != "" {
		t.Fatalf("system path: %q %v", r, err)
	}
	if r, err := RootFromExecutable("/home/ops/edge/bin/syedge-wrapper"); err != nil || r != "/home/ops/edge" {
		t.Fatalf("user path: %q %v", r, err)
	}
	for _, bad := range []string{"/tmp/syedge-wrapper", "/home/ops/edge/sbin/syedge-wrapper", "/home/ops/edge/bin/other", "/bin/syedge-wrapper"} {
		if _, err := RootFromExecutable(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckInstanceUser(t *testing.T) {
	root := filepath.Join(t.TempDir(), "edge")
	me := os.Getuid()
	write := func(prefix, body string) {
		t.Helper()
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, InstanceFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root, `{"mode":"user","root":"`+root+`","http_port":8080,"https_port":8443,"status_port":8082}`)
	if err := CheckInstanceUser(root, "", me); err != nil {
		t.Fatalf("user default instance: %v", err)
	}
	write(filepath.Join(root, "instances", "blue"), `{"instance":"blue","mode":"user","root":"/somewhere/else"}`)
	if err := CheckInstanceUser(root, "blue", me); err == nil || !strings.Contains(err.Error(), "install root") {
		t.Fatalf("root mismatch: %v", err)
	}
	write(filepath.Join(root, "instances", "green"), `{"instance":"green"}`)
	if err := CheckInstanceUser(root, "green", me); err == nil || !strings.Contains(err.Error(), "system-mode") {
		t.Fatalf("system instance under user root: %v", err)
	}
	if err := CheckInstanceUser(root, "", me+1); err == nil {
		t.Fatal("instance owned by another user accepted")
	}
}
