package wrapper

import (
	"errors"
	"reflect"
	"testing"
)

func TestBuildAllowed(t *testing.T) {
	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"test", "r000123"}, []string{"-t", "-q", "-c", "/opt/syapp/conf/releases/r000123/syapp.conf"}},
		{[]string{"test", "r123456789012"}, []string{"-t", "-q", "-c", "/opt/syapp/conf/releases/r123456789012/syapp.conf"}},
		{[]string{"reload"}, []string{"-s", "reload"}},
		{[]string{"reopen"}, []string{"-s", "reopen"}},
		{[]string{"quit"}, []string{"-s", "quit"}},
	}
	for _, c := range cases {
		cmd, err := Build(c.argv)
		if err != nil {
			t.Fatalf("%v: unexpected error %v", c.argv, err)
		}
		if cmd.Path != SyappBin {
			t.Fatalf("%v: path = %q", c.argv, cmd.Path)
		}
		if !reflect.DeepEqual(cmd.Args, c.want) {
			t.Fatalf("%v: args = %q, want %q", c.argv, cmd.Args, c.want)
		}
		if !reflect.DeepEqual(cmd.Env, SafeEnv) {
			t.Fatalf("%v: env not sanitized", c.argv)
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
	}
	for _, argv := range bad {
		if _, err := Build(argv); !errors.Is(err, ErrUsage) {
			t.Fatalf("%q: expected ErrUsage, got %v", argv, err)
		}
	}
}
