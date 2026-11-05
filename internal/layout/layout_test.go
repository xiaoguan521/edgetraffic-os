package layout

import "testing"

// 默认品牌、system 模式下的路径必须与重构前写死的值逐字相同，保证本次重构不改变任何已部署节点的行为。
func TestSystemDefaultMatchesLegacyPaths(t *testing.T) {
	l := System("")
	for _, c := range []struct{ name, got, want string }{
		{"Prefix", l.Prefix(), "/opt/syapp"},
		{"ConfRoot", l.ConfRoot(), "/opt/syapp/conf"},
		{"ReleasesDir", l.ReleasesDir(), "/opt/syapp/conf/releases"},
		{"CurrentConf", l.CurrentConf(), "/opt/syapp/conf/current/syapp.conf"},
		{"Bin", l.Bin(), "/opt/syapp/sbin/syapp"},
		{"Versions", l.Versions(), "/opt/syapp/versions"},
		{"Modules", l.Modules(), "/opt/syapp/modules"},
		{"PidFile", l.PidFile(), "/run/syapp/syapp.pid"},
		{"ErrorLog", l.ErrorLog(), "/var/log/syapp/error.log"},
		{"AccessLog", l.AccessLog(), "/var/log/syapp/access.json"},
		{"AgentState", l.AgentState(), "/var/lib/syapp-agent"},
		{"AgentLock", l.AgentLock(), "/run/syapp-agent/apply.lock"},
		{"UpgradeIncoming", l.UpgradeIncoming(), "/var/lib/syapp-agent/upgrade"},
		{"WrapperBin", l.WrapperBin(), "/usr/libexec/syapp/syapp-wrapper"},
		{"AgentBin", l.AgentBin(), "/usr/bin/syapp-agent"},
		{"StatusURL", l.StatusURL(8081), "http://127.0.0.1:8081/__syapp_status"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// 具名实例的路径与提交 9b0716a 中 wrapper 已使用的路径一致（设计 0001 第 3.3 节）。
func TestSystemInstancePaths(t *testing.T) {
	l := System("blue")
	for _, c := range []struct{ name, got, want string }{
		{"Prefix", l.Prefix(), "/opt/syapp/instances/blue"},
		{"ReleasesDir", l.ReleasesDir(), "/opt/syapp/instances/blue/conf/releases"},
		{"CurrentConf", l.CurrentConf(), "/opt/syapp/instances/blue/conf/current/syapp.conf"},
		{"Bin", l.Bin(), "/opt/syapp/sbin/syapp"},
		{"Versions", l.Versions(), "/opt/syapp/versions"},
		{"PidFile", l.PidFile(), "/run/syapp-blue/syapp.pid"},
		{"ErrorLog", l.ErrorLog(), "/var/log/syapp-blue/error.log"},
		{"AgentState", l.AgentState(), "/var/lib/syapp-agent-blue"},
		{"AgentLock", l.AgentLock(), "/run/syapp-agent-blue/apply.lock"},
		{"UpgradeIncoming", l.UpgradeIncoming(), "/var/lib/syapp-agent-blue/upgrade"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestUserModePathsStayUnderRoot(t *testing.T) {
	l, err := User("/home/ops/syedge", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"Prefix", l.Prefix(), "/home/ops/syedge"},
		{"Bin", l.Bin(), "/home/ops/syedge/sbin/syapp"},
		{"PidFile", l.PidFile(), "/home/ops/syedge/run/syapp.pid"},
		{"ErrorLog", l.ErrorLog(), "/home/ops/syedge/logs/error.log"},
		{"AgentState", l.AgentState(), "/home/ops/syedge/agent"},
		{"AgentLock", l.AgentLock(), "/home/ops/syedge/run/agent.lock"},
		{"WrapperBin", l.WrapperBin(), "/home/ops/syedge/bin/syapp-wrapper"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	li, _ := User("/home/ops/syedge", "blue")
	if got := li.PidFile(); got != "/home/ops/syedge/instances/blue/run/syapp.pid" {
		t.Errorf("instance PidFile = %q", got)
	}
	if got := li.Bin(); got != "/home/ops/syedge/sbin/syapp" {
		t.Errorf("instance Bin must be shared, got %q", got)
	}
}

func TestInvalidInputs(t *testing.T) {
	for _, bad := range []string{"default", "Blue", "1abc", "a/b", "../x", "a b", "-x", "abcdefghijklmnopq"} {
		if ValidInstance(bad) {
			t.Errorf("instance %q accepted", bad)
		}
	}
	for _, ok := range []string{"", "blue", "web-1", "a"} {
		if !ValidInstance(ok) {
			t.Errorf("instance %q rejected", ok)
		}
	}
	for _, root := range []string{"", "relative/dir", "/", "/home/ops/../etc", "/home/o ps"} {
		if _, err := User(root, ""); err == nil {
			t.Errorf("user root %q accepted", root)
		}
	}
	if _, err := User("/home/ops/x", "Bad"); err == nil {
		t.Error("bad instance accepted in user mode")
	}
}

func TestInstanceNames(t *testing.T) {
	d, b := System(""), System("blue")
	for _, c := range []struct{ got, want string }{
		{d.AgentUser(), "syapp-agent"}, {b.AgentUser(), "syapp-agent-blue"},
		{d.DataplaneUnit(), "syapp.service"}, {b.DataplaneUnit(), "syapp@blue.service"},
		{d.AgentUnit(), "syapp-agent.service"}, {b.AgentUnit(), "syapp-agent@blue.service"},
		{b.SudoersFile(), "/etc/sudoers.d/syapp-agent-blue"}, {d.SudoersFile(), "/etc/sudoers.d/syapp-agent"},
		{b.InstanceFile(), "/opt/syapp/instances/blue/instance.json"},
		{Group(), "syapp"}, {WorkerUser(), "syapp"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	// Linux 用户名最长 32：最长的实例名加前缀也在范围内
	if n := len(System("abcdefghijklmnop").AgentUser()); n > 32 {
		t.Fatalf("agent user name too long: %d", n)
	}
}
