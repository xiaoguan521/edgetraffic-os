package layout

import "testing"

// 默认品牌、system 模式下的路径必须与重构前写死的值逐字相同，保证本次重构不改变任何已部署节点的行为。
func TestSystemDefaultMatchesLegacyPaths(t *testing.T) {
	l := System("")
	for _, c := range []struct{ name, got, want string }{
		{"Prefix", l.Prefix(), "/opt/syedge"},
		{"ConfRoot", l.ConfRoot(), "/opt/syedge/conf"},
		{"ReleasesDir", l.ReleasesDir(), "/opt/syedge/conf/releases"},
		{"CurrentConf", l.CurrentConf(), "/opt/syedge/conf/current/syedge.conf"},
		{"Bin", l.Bin(), "/opt/syedge/sbin/syedge"},
		{"Versions", l.Versions(), "/opt/syedge/versions"},
		{"Modules", l.Modules(), "/opt/syedge/modules"},
		{"PidFile", l.PidFile(), "/run/syedge/syedge.pid"},
		{"ErrorLog", l.ErrorLog(), "/var/log/syedge/error.log"},
		{"AccessLog", l.AccessLog(), "/var/log/syedge/access.json"},
		{"AgentState", l.AgentState(), "/var/lib/syedge-agent"},
		{"AgentLock", l.AgentLock(), "/run/syedge-agent/apply.lock"},
		{"UpgradeIncoming", l.UpgradeIncoming(), "/var/lib/syedge-agent/upgrade"},
		{"WrapperBin", l.WrapperBin(), "/usr/libexec/syedge/syedge-wrapper"},
		{"AgentBin", l.AgentBin(), "/usr/bin/syedge-agent"},
		{"StatusURL", l.StatusURL(8081), "http://127.0.0.1:8081/__syedge_status"},
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
		{"Prefix", l.Prefix(), "/opt/syedge/instances/blue"},
		{"ReleasesDir", l.ReleasesDir(), "/opt/syedge/instances/blue/conf/releases"},
		{"CurrentConf", l.CurrentConf(), "/opt/syedge/instances/blue/conf/current/syedge.conf"},
		{"Bin", l.Bin(), "/opt/syedge/sbin/syedge"},
		{"Versions", l.Versions(), "/opt/syedge/versions"},
		{"PidFile", l.PidFile(), "/run/syedge-blue/syedge.pid"},
		{"ErrorLog", l.ErrorLog(), "/var/log/syedge-blue/error.log"},
		{"AgentState", l.AgentState(), "/var/lib/syedge-agent-blue"},
		{"AgentLock", l.AgentLock(), "/run/syedge-agent-blue/apply.lock"},
		{"UpgradeIncoming", l.UpgradeIncoming(), "/var/lib/syedge-agent-blue/upgrade"},
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
		{"Bin", l.Bin(), "/home/ops/syedge/sbin/syedge"},
		{"PidFile", l.PidFile(), "/home/ops/syedge/run/syedge.pid"},
		{"ErrorLog", l.ErrorLog(), "/home/ops/syedge/logs/error.log"},
		{"AgentState", l.AgentState(), "/home/ops/syedge/agent"},
		{"AgentLock", l.AgentLock(), "/home/ops/syedge/run/agent.lock"},
		{"WrapperBin", l.WrapperBin(), "/home/ops/syedge/bin/syedge-wrapper"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	li, _ := User("/home/ops/syedge", "blue")
	if got := li.PidFile(); got != "/home/ops/syedge/instances/blue/run/syedge.pid" {
		t.Errorf("instance PidFile = %q", got)
	}
	if got := li.Bin(); got != "/home/ops/syedge/sbin/syedge" {
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
		{d.AgentUser(), "syedge-agent"}, {b.AgentUser(), "syedge-agent-blue"},
		{d.DataplaneUnit(), "syedge.service"}, {b.DataplaneUnit(), "syedge@blue.service"},
		{d.AgentUnit(), "syedge-agent.service"}, {b.AgentUnit(), "syedge-agent@blue.service"},
		{b.SudoersFile(), "/etc/sudoers.d/syedge-agent-blue"}, {d.SudoersFile(), "/etc/sudoers.d/syedge-agent"},
		{b.InstanceFile(), "/opt/syedge/instances/blue/instance.json"},
		{Group(), "syedge"}, {WorkerUser(), "syedge"},
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
