package wrapper

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestBuildAllowed(t *testing.T) {
	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"test", "r000123"}, []string{"-t", "-q", "-c", "/opt/syedge/conf/releases/r000123/syedge.conf"}},
		{[]string{"test", "r123456789012"}, []string{"-t", "-q", "-c", "/opt/syedge/conf/releases/r123456789012/syedge.conf"}},
		{[]string{"reload"}, []string{"-s", "reload"}},
		{[]string{"reopen"}, []string{"-s", "reopen"}},
		{[]string{"quit"}, []string{"-s", "quit"}},
		{[]string{"--instance", "edge-a", "test", "r000123"}, []string{"-t", "-q", "-c", "/opt/syedge/instances/edge-a/conf/releases/r000123/syedge.conf"}},
		{[]string{"--instance", "edge-2", "reload"}, []string{"-c", "/opt/syedge/instances/edge-2/conf/current/syedge.conf", "-s", "reload"}},
		{[]string{"--instance", "edge-2", "reopen"}, []string{"-c", "/opt/syedge/instances/edge-2/conf/current/syedge.conf", "-s", "reopen"}},
		{[]string{"--instance", "edge-2", "quit"}, []string{"-c", "/opt/syedge/instances/edge-2/conf/current/syedge.conf", "-s", "quit"}},
	}
	for _, c := range cases {
		cmd, err := Build(c.argv)
		if err != nil {
			t.Fatalf("%v: unexpected error %v", c.argv, err)
		}
		if cmd.Path != DataplaneBin() {
			t.Fatalf("%v: path = %q", c.argv, cmd.Path)
		}
		if len(c.want) > 1 && c.want[0] == "-t" {
			c.want = append(c.want[:2], append([]string{"-p", "/opt/syedge"}, c.want[2:]...)...)
		} else {
			c.want = append([]string{"-p", "/opt/syedge"}, c.want...)
		}
		if !reflect.DeepEqual(cmd.Args, c.want) {
			t.Fatalf("%v: args = %q, want %q", c.argv, cmd.Args, c.want)
		}
		if !reflect.DeepEqual(cmd.Env, SafeEnv) {
			t.Fatalf("%v: env not sanitized", c.argv)
		}
	}
}

func TestUserCommandsUseTheirRuntimePrefix(t *testing.T) {
	root := t.TempDir()
	cmd, err := BuildAt(root, []string{"test", "r000123"})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range cmd.Args {
		if arg == "-p" && i+1 < len(cmd.Args) && cmd.Args[i+1] == root {
			return
		}
	}
	t.Fatalf("configuration test has no trusted runtime prefix %q: %q", root, cmd.Args)
}

func TestUpgradeMasterPrefixCompatibility(t *testing.T) {
	for _, tc := range []struct {
		title, compiled, want string
		ok                    bool
	}{
		{"SyEdge: master process /opt/syedge/sbin/syedge", "/home/atwasoft/syedge", "/opt/syedge", false},
		{"SyEdge: master process /opt/syedge/sbin/syedge", "/opt/syedge", "/opt/syedge", true},
		{"SyEdge: master process /opt/syedge/sbin/syedge -p /opt/syedge", "/home/atwasoft/syedge", "/opt/syedge", true},
		{"SyEdge: master process /opt/syedge/sbin/syedge -p /wrong", "/opt/syedge", "/opt/syedge", false},
	} {
		if got := compatibleMasterPrefix(tc.title, tc.compiled, tc.want); got != tc.ok {
			t.Errorf("title=%q compiled=%q: got %t, want %t", tc.title, tc.compiled, got, tc.ok)
		}
	}
}

func TestBuildUpgradeAllowed(t *testing.T) {
	for argv, want := range map[string]Command{
		"upgrade-start 1.30.6":   {Upgrade: "start", Version: "1.30.6"},
		"upgrade-start 1.31.0-2": {Upgrade: "start", Version: "1.31.0-2"},
		"upgrade-commit":         {Upgrade: "commit"},
		"upgrade-abort":          {Upgrade: "abort"},
	} {
		got, err := Build(strings.Fields(argv))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v %v", argv, got, err)
		}
	}
}

// 规格书 5.5 / 12.1 P2：wrapper 注入用例必须全部拒绝。
func TestBuildRejected(t *testing.T) {
	bad := [][]string{
		nil,
		{},
		{"restart"},
		{"stop"},
		{"test"},
		{"test", "r12345"},                 // 太短
		{"test", "r1234567890123"},         // 太长
		{"test", "000123"},                 // 缺前缀
		{"test", "r000123/../../etc"},      // 穿越
		{"test", "../r000123"},             // 穿越
		{"test", "r000123 -g daemon off;"}, // 空格注入
		{"test", "r000123\n"},              // 换行
		{"test", "r000123", "-g", "x"},     // 多余参数
		{"test", "-c", "/tmp/evil.conf"},
		{"reload", "-g", "daemon off;"},
		{"reload", "now"},
		{"-s", "reload"},
		{"-c", "/tmp/x.conf"},
		{"-p", "/tmp"},
		{"sh", "-c", "id"},
		// 升级子命令（规格书 12.8）
		{"upgrade-start"},
		{"upgrade-start", "1.30"},
		{"upgrade-start", "1.30.6-"},
		{"upgrade-start", "v1.30.6"},
		{"upgrade-start", "1.30.6-1-2"},
		{"upgrade-start", "1.30.6;id"},
		{"upgrade-start", "1.30.6 "},
		{"upgrade-start", "1.30.6\n"},
		{"upgrade-start", "../../1.30.6"},
		{"upgrade-start", "1.30.6/../../etc"},
		{"upgrade-start", "1.30.6", "extra"},
		{"upgrade-start", "-c"},
		{"upgrade-commit", "now"},
		{"upgrade-abort", "1.30.6"},
		{"upgrade-restart"},
		{"upgrade"},
		{"--instance"},
		{"--instance", ""},
		{"--instance", "../evil", "reload"},
		{"--instance", "EdgeUpper", "reload"},
		{"--instance", "-dashstart", "reload"},
		{"--instance", "edge-a"},
		{"--instance", "edge_2", "reload"},  // 下划线不在实例名规则内（internal/layout）
		{"--instance", "1edge", "reload"},   // 必须以字母开头
		{"--instance", "default", "reload"}, // 保留名
		{"--instance", "abcdefghijklmnopq", "reload"},
		{"reload", "--instance", "edge-a"}, // --instance 只能是第一个参数
		{"--instance", "edge-a", "--instance", "edge-b", "reload"},
		{"--instance", "edge-a", "test", "-c", "/tmp/evil.conf"},
	}
	for _, argv := range bad {
		if _, err := Build(argv); !errors.Is(err, ErrUsage) {
			t.Fatalf("%q: expected ErrUsage, got %v", argv, err)
		}
	}
}
