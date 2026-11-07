package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 兼容模式（与传统 Web 服务器相同：master 以 root 运行，worker 降为数据面用户）：系统不支持 ambient 能力时 nonroot 自动进入，
// 写标记与单元附加配置，wrapper 以 root 运行（sudoers 为 (root)），不做无中断切换。
func TestNonrootRootMasterWhenNoAmbient(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the marker is trusted only when owned by root (CI runs this as root in the dataplane acceptance)")
	}
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
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
	for _, p := range []string{"/etc/systemd/system/syedge.service.d/root-master.conf", "/etc/systemd/system/syedge@.service.d/root-master.conf"} {
		b, err := os.ReadFile(f.path(p))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		// master 以 root 运行；边界集先清空再列出 master 用到的能力（只清空会让 root 也无法绑定 80）
		for _, want := range []string{"User=root\n", "Group=root\n", "CapabilityBoundingSet=\n", "CAP_NET_BIND_SERVICE", "CAP_SETUID", "NoNewPrivileges=no"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s missing %q:\n%s", p, want, s)
			}
		}
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
