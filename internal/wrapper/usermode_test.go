package wrapper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// user 模式（设计 0001 第 6.2 节）：同一套白名单，路径来自安装根目录，每条命令都带 -e <实例错误日志>。
func TestBuildUserMode(t *testing.T) {
	root := "/home/ops/edge"
	for _, c := range []struct {
		argv []string
		path string
		args []string
	}{
		{[]string{"test", "r000123"}, root + "/sbin/syapp", []string{"-t", "-q", "-e", root + "/logs/error.log", "-c", root + "/conf/releases/r000123/syapp.conf"}},
		{[]string{"reload"}, root + "/sbin/syapp", []string{"-e", root + "/logs/error.log", "-c", root + "/conf/current/syapp.conf", "-s", "reload"}},
		{[]string{"--instance", "blue", "quit"}, root + "/sbin/syapp", []string{"-e", root + "/instances/blue/logs/error.log", "-c", root + "/instances/blue/conf/current/syapp.conf", "-s", "quit"}},
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
	if cmd, _ := Build([]string{"reload"}); strings.Join(cmd.Args, " ") != "-s reload" || cmd.Root != "" {
		t.Errorf("system reload = %q", cmd.Args)
	}
	p, err := DefaultPathsAt(root, "blue")
	if err != nil || p.Bin != root+"/sbin/syapp" || p.PidFile != root+"/instances/blue/run/syapp.pid" || p.ErrorLog != root+"/instances/blue/logs/error.log" {
		t.Fatalf("user upgrade paths = %+v %v", p, err)
	}
	if sp := DefaultPathsForInstance(""); sp.ErrorLog != "" {
		t.Fatal("system mode must not pass -e")
	}
}

func TestRootFromExecutable(t *testing.T) {
	if r, err := RootFromExecutable("/usr/libexec/syapp/syapp-wrapper"); err != nil || r != "" {
		t.Fatalf("system path: %q %v", r, err)
	}
	if r, err := RootFromExecutable("/home/ops/edge/bin/syapp-wrapper"); err != nil || r != "/home/ops/edge" {
		t.Fatalf("user path: %q %v", r, err)
	}
	for _, bad := range []string{"/tmp/syapp-wrapper", "/home/ops/edge/sbin/syapp-wrapper", "/home/ops/edge/bin/other", "/bin/syapp-wrapper"} {
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
