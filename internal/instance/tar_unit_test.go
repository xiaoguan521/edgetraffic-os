package instance

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// tarInstall 模拟 tar 包 install.sh：把软件包单元直接装在 /etc/systemd/system，没有 rpm/deb 目录里的副本。
func tarInstall(t *testing.T, f *fakeEnv) (units []string, pristine map[string][]byte) {
	t.Helper()
	pristine = map[string][]byte{}
	_ = os.MkdirAll(f.path("/etc/systemd/system"), 0o755)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	for _, u := range []string{"syedge.service", "syedge@.service", "syedge-firewall.service", "syedge-firewall-refresh.service"} {
		b, err := os.ReadFile(filepath.Join("../../dataplane/packaging/systemd", u))
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.ReplaceAll(string(b), "/opt/syedge", layout.DefaultSystemRoot()))
		if err := os.WriteFile(f.path("/etc/systemd/system/"+u), b, 0o644); err != nil {
			t.Fatal(err)
		}
		units = append(units, u)
		pristine[u] = b
	}
	return units, pristine
}

func hasRootUser(b []byte) bool { return strings.Contains(string(b), "\nUser=root\n") }

func enterAndLeaveCompat(t *testing.T, f *fakeEnv) {
	t.Helper()
	f.RootMaster = true
	f.Ambient = func() bool { return true }
	f.Handover = func(string) error { return nil }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path(layout.RootMasterMarker())); err != nil {
		t.Fatal(err)
	}
	f.RootMaster = false
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
}

// tar 安装：进入兼容模式把 /etc 里的单元原位改成 User=root；切回普通模式后必须恢复成软件包原样，
// 不能把改写过的单元当作“软件包单元”再次使用。
func TestTarInstallLeavingCompatRestoresThePackageUnit(t *testing.T) {
	f := newFake(t)
	units, pristine := tarInstall(t, f)
	f.RootMaster = true
	f.Ambient = func() bool { return true }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service"))
	if !hasRootUser(b) {
		t.Fatal("precondition: compat mode did not write User=root")
	}
	_ = os.Remove(f.path(layout.RootMasterMarker()))
	f.RootMaster = false
	f.Handover = func(string) error { return nil }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		got, err := os.ReadFile(f.path("/etc/systemd/system/" + u))
		if err != nil {
			t.Fatalf("%s missing after leaving compat mode: %v", u, err)
		}
		if hasRootUser(got) {
			t.Errorf("%s still runs as root after leaving compat mode", u)
		}
		if _, custom := f.customSystemRoot(); !custom && string(got) != string(pristine[u]) {
			t.Errorf("%s differs from the package unit", u)
		}
	}
}

// 来回切换多次，结果不变。
func TestTarInstallCompatSwitchRepeats(t *testing.T) {
	f := newFake(t)
	tarInstall(t, f)
	enterAndLeaveCompat(t, f)
	first, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service"))
	enterAndLeaveCompat(t, f)
	second, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service"))
	if string(first) != string(second) || hasRootUser(second) {
		t.Fatalf("repeat switch changed the unit or kept root:\n%s", second)
	}
}

// 重新执行 install.sh（升级）会带来新版单元；记录的原始副本必须跟着更新，否则之后切回的是旧版本。
func TestTarReinstallRefreshesThePristineCopy(t *testing.T) {
	f := newFake(t)
	tarInstall(t, f)
	f.RootMaster = true
	f.Ambient = func() bool { return true }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(f.path(layout.RootMasterMarker()))
	f.RootMaster = false
	f.Handover = func(string) error { return nil }
	// install.sh 用新版单元覆盖 /etc 里的文件
	_, pristine := tarInstall(t, f)
	newer := string(pristine["syedge.service"]) + "# newer release\n"
	if err := os.WriteFile(f.path("/etc/systemd/system/syedge.service"), []byte(newer), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	enterAndLeaveCompat(t, f)
	got, _ := os.ReadFile(f.path("/etc/systemd/system/syedge.service"))
	if !strings.Contains(string(got), "# newer release") || hasRootUser(got) {
		t.Fatalf("pristine copy was not refreshed on reinstall:\n%s", got)
	}
}

// 本修复之前已进入兼容模式的 tar 节点没有原始副本：切回时不得悄悄保留 User=root 还报告成功，必须明确提示重新执行 install.sh。
func TestTarCompatWithoutPristineCopyWarns(t *testing.T) {
	f := newFake(t)
	_, pristine := tarInstall(t, f)
	gen := compatUnit(string(pristine["syedge.service"]), rootMasterCaps, "syedge")
	if err := os.WriteFile(f.path("/etc/systemd/system/syedge.service"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	f.Handover = func(string) error { return nil }
	f.Ambient = func() bool { return true }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if out := f.Out.(*bytes.Buffer).String(); !strings.Contains(out, "install.sh") {
		t.Fatalf("no instruction to rerun install.sh:\n%s", out)
	}
}
