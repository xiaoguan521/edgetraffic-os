package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 兼容模式（与传统 Web 服务器相同：master 以 root 运行，worker 降为数据面用户）：系统不支持 ambient 能力时 nonroot 自动进入，
// 写标记与覆盖单元，wrapper 以 root 运行（sudoers 为 (root)），不做无中断切换。
func TestNonrootRootMasterWhenNoAmbient(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the marker is trusted only when owned by root (CI runs this as root in the dataplane acceptance)")
	}
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	vendorUnits(t, f)
	// v0.4.1 写入的附加配置（systemd 208 上会让 master 丢掉 CAP_SETGID）
	for _, u := range []string{"syedge.service", "syedge@.service"} {
		_ = os.MkdirAll(f.path("/etc/systemd/system/"+u+".d"), 0o755)
		_ = os.WriteFile(f.path("/etc/systemd/system/"+u+".d/root-master.conf"), []byte("[Service]\nCapabilityBoundingSet=\n"), 0o644)
	}
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path("/etc/sudoers.d/syedge-agent"), []byte("syedge-agent ALL=(syedge) NOPASSWD: /usr/libexec/syedge/syedge-wrapper reload\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	handed := 0
	f.Handover = func(string) error { handed++; return nil }
	f.Ambient = func() bool { return false }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if handed != 0 {
		t.Fatal("root-master mode must not hand the master over to the data-plane user")
	}
	if !f.rootMaster() {
		t.Fatal("marker not written")
	}
	// 软件包单元原样保留；覆盖单元在 /etc/systemd/system，只有一行能力边界（systemd 208 对多行取交集），旧版附加配置被删除
	for _, u := range []string{"syedge.service", "syedge@.service"} {
		b, err := os.ReadFile(f.path("/etc/systemd/system/" + u))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{"# syedge-instance nonroot 生成", "[Service]\nUser=root\nGroup=root\nCapabilityBoundingSet=CAP_CHOWN", "CAP_SETGID", "CAP_NET_BIND_SERVICE", "NoNewPrivileges=no\n", "ExecStart=/opt/syedge/sbin/syedge"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s missing %q:\n%s", u, want, s)
			}
		}
		for _, bad := range []string{"User=syedge", "AmbientCapabilities", "NoNewPrivileges=yes", "CapabilityBoundingSet=\n"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s still has %q:\n%s", u, bad, s)
			}
		}
		if strings.Count(s, "CapabilityBoundingSet=") != 1 {
			t.Errorf("%s: want exactly one CapabilityBoundingSet line:\n%s", u, s)
		}
		if _, err := os.Stat(f.path("/etc/systemd/system/" + u + ".d/root-master.conf")); err == nil {
			t.Errorf("legacy drop-in for %s not removed", u)
		}
		if v, _ := os.ReadFile(f.path("/usr/lib/systemd/system/" + u)); !strings.Contains(string(v), "User=syedge") {
			t.Errorf("vendor unit %s modified", u)
		}
	}
	// 可重复执行：再执行一次结果相同
	first, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service"))
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service")); string(again) != string(first) {
		t.Fatalf("not idempotent:\n%s\n---\n%s", first, again)
	}
	for _, p := range []string{"/etc/sudoers.d/syedge-agent", "/etc/sudoers.d/syedge-agent-blue"} {
		b, _ := os.ReadFile(f.path(p))
		if !strings.Contains(string(b), "=(root)") || strings.Contains(string(b), "=(syedge)") {
			t.Errorf("%s not switched to (root):\n%s", p, b)
		}
	}
	// 兼容模式下 reopen-all 以 root 发信号（master 是 root），不经 runuser
	f.cmds = nil
	if err := f.ReopenAll(); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "runuser") {
			t.Fatalf("reopen-all in root-master mode: %q", c)
		}
	}
	// 再添加实例：校验直接以 root 执行 wrapper
	f.cmds = nil
	green := render.Profile{Instance: "green", HTTPPort: 28080, HTTPSPort: 28443, StatusPort: 8083}
	if err := f.Add(AddOptions{Profile: green}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "syedge-wrapper") && strings.HasPrefix(c, "runuser") {
			t.Fatalf("root-master add validates through runuser: %q", c)
		}
	}
}

// 支持 ambient 的系统也可以显式选择兼容模式（--root-master）。
func TestNonrootRootMasterForced(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the marker is trusted only when owned by root (CI runs this as root in the dataplane acceptance)")
	}
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	f.RootMaster = true
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if !f.rootMaster() {
		t.Fatal("forced root-master mode not recorded")
	}
}

// vendorUnits 把仓库中的单元文件放到模拟根的软件包目录。
func vendorUnits(t *testing.T, f *fakeEnv) {
	t.Helper()
	for _, u := range []string{"syedge.service", "syedge@.service", "syedge-firewall.service", "syedge-firewall-refresh.service"} {
		b, err := os.ReadFile("../../dataplane/packaging/systemd/" + u)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(f.path("/usr/lib/systemd/system"), 0o755)
		if err := os.WriteFile(f.path("/usr/lib/systemd/system/"+u), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 离开兼容模式：删除标记后 nonroot 删除覆盖单元，软件包单元重新生效。
func TestNonrootLeavesRootMaster(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the marker is trusted only when owned by root")
	}
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	vendorUnits(t, f)
	f.RootMaster = true
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("/etc/systemd/system/syedge.service")); err != nil {
		t.Fatal("override not written")
	}
	_ = os.Remove(f.path(layout.RootMasterMarker()))
	f.RootMaster = false
	f.Handover = func(string) error { return nil }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"syedge.service", "syedge@.service", "syedge-firewall.service"} {
		if _, err := os.Stat(f.path("/etc/systemd/system/" + u)); err == nil {
			t.Errorf("override %s left after leaving root-master mode", u)
		}
	}
}

// tar 包安装：单元直接装在 /etc/systemd/system，覆盖单元在原文件上生成（重复执行结果不变）。
func TestCompatUnitInPlace(t *testing.T) {
	src, err := os.ReadFile("../../dataplane/packaging/systemd/syedge.service")
	if err != nil {
		t.Fatal(err)
	}
	once := compatUnit(string(src), rootMasterCaps, "syedge")
	if twice := compatUnit(once, rootMasterCaps, "syedge"); twice != once {
		t.Fatalf("compatUnit not idempotent:\n%s\n---\n%s", once, twice)
	}
	if strings.Count(once, "[Service]\nUser=root\n") != 1 || strings.Contains(once, "Ambient") {
		t.Fatalf("unexpected unit:\n%s", once)
	}
}
