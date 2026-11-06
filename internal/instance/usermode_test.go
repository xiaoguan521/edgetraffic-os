package instance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

const uroot = "/home/ops/edge"

type spawned struct {
	pidFile, name string
	args          []string
}

func newUserFake(t *testing.T, systemd bool) (*fakeEnv, *[]spawned) {
	t.Helper()
	f := newFake(t)
	f.UserRoot, f.Home = uroot, "/home/ops"
	f.UserSystemd = func() bool { return systemd }
	var sp []spawned
	f.Spawn = func(pidFile, logFile, name string, args ...string) error {
		sp = append(sp, spawned{pidFile, name, args})
		return os.WriteFile(pidFile, []byte("777\n"), 0o644)
	}
	// 模拟 user 模式 bootstrap：写出当前配置
	run := f.Run
	f.Run = func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "bootstrap" {
			inst := ""
			for i := range args {
				if args[i] == "-instance" {
					inst = args[i+1]
				}
			}
			l, _ := layout.User(uroot, inst)
			_ = os.MkdirAll(filepath.Dir(f.path(l.CurrentConf())), 0o755)
			_ = os.WriteFile(f.path(l.CurrentConf()), []byte("# baseline\n"), 0o644)
		}
		return run(name, args...)
	}
	return f, &sp
}

var userPorts = render.Profile{HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8082}

func TestUserAddDefaultWithoutSystemd(t *testing.T) {
	f, _ := newUserFake(t, false)
	if err := f.Add(AddOptions{Profile: userPorts}); err != nil {
		t.Fatal(err)
	}
	l, _ := layout.User(uroot, "")
	b, err := os.ReadFile(f.path(l.InstanceFile()))
	if err != nil {
		t.Fatal(err)
	}
	var p render.Profile
	_ = json.Unmarshal(b, &p)
	if p.Mode != "user" || p.Root != uroot || p.HTTPPort != 8080 {
		t.Fatalf("instance.json = %s", b)
	}
	for _, d := range []string{l.ReleasesDir(), l.LogDir(), l.CacheDir(), l.RunDir(), l.AgentState()} {
		if _, err := os.Stat(f.path(d)); err != nil {
			t.Errorf("missing %s", d)
		}
	}
	// 没有任何需要 root 的操作
	for _, c := range f.cmds {
		for _, banned := range []string{"useradd", "visudo", "runuser", "chown", "sudo"} {
			if strings.HasPrefix(c, banned) {
				t.Errorf("user mode ran %q", c)
			}
		}
	}
	for _, want := range []string{
		uroot + "/bin/syedge-agent bootstrap -root " + uroot,
		uroot + "/sbin/syedge -t -q -e " + uroot + "/logs/error.log -c " + uroot + "/conf/current/syedge.conf",
		uroot + "/sbin/syedge -e " + uroot + "/logs/error.log -c " + uroot + "/conf/current/syedge.conf",
	} {
		if !f.ran(want) {
			t.Errorf("missing command %q in:\n%s", want, strings.Join(f.cmds, "\n"))
		}
	}
	if out := f.Out.(*bytes.Buffer).String(); !strings.Contains(out, "@reboot "+uroot+"/bin/syedge-instance --root "+uroot+" ensure") {
		t.Errorf("cron instructions missing:\n%s", out)
	}
}

func TestUserAddRejectsPrivilegedPorts(t *testing.T) {
	f, _ := newUserFake(t, false)
	if err := f.Add(AddOptions{Profile: render.Profile{HTTPPort: 80, HTTPSPort: 8443, StatusPort: 8082}}); err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("port 80 in user mode: %v", err)
	}
	if err := f.Add(AddOptions{Profile: render.Profile{}}); err == nil {
		t.Fatal("default ports (80/443) accepted in user mode")
	}
}

func TestUserAddNamedWithSystemdAndEnroll(t *testing.T) {
	f, _ := newUserFake(t, true)
	if err := f.Add(AddOptions{Profile: userPorts}); err != nil {
		t.Fatal(err)
	}
	blueP := render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8083}
	if err := f.Add(AddOptions{Profile: blueP, Console: "c:9443", Token: "tok", CAFingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(f.path("/home/ops/.config/systemd/user/syedge-blue.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart="+uroot+"/sbin/syedge -e "+uroot+"/instances/blue/logs/error.log -c "+uroot+"/instances/blue/conf/current/syedge.conf") {
		t.Fatalf("unit:\n%s", unit)
	}
	ag, _ := os.ReadFile(f.path("/home/ops/.config/systemd/user/syedge-agent-blue.service"))
	if !strings.Contains(string(ag), "ExecStart="+uroot+"/bin/syedge-agent run -root "+uroot+" -instance blue") || strings.Contains(string(ag), "User=") {
		t.Fatalf("agent unit:\n%s", ag)
	}
	for _, want := range []string{
		"systemctl --user enable --now syedge-blue.service",
		uroot + "/bin/syedge-agent enroll -root " + uroot + " -instance blue -server c:9443 -token tok -ca-fingerprint fp",
		"systemctl --user enable --now syedge-agent-blue.service",
	} {
		if !f.ran(want) {
			t.Errorf("missing %q", want)
		}
	}
	// 同根目录下与默认实例端口冲突
	if err := f.Add(AddOptions{Profile: render.Profile{Instance: "red", HTTPPort: 8080, HTTPSPort: 28443, StatusPort: 8090}}); err == nil {
		t.Fatal("port clash within the user root accepted")
	}
}

func TestEnsure(t *testing.T) {
	f, sp := newUserFake(t, false)
	if err := f.Add(AddOptions{Profile: userPorts}); err != nil {
		t.Fatal(err)
	}
	l, _ := layout.User(uroot, "")
	// 模拟已注册
	_ = os.WriteFile(f.path(filepath.Join(l.AgentState(), "node.json")), []byte("{}"), 0o600)
	f.cmds, *sp = nil, nil
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	if !f.ran(uroot+"/sbin/syedge -e") || len(*sp) != 1 || (*sp)[0].name != uroot+"/bin/syedge-agent" {
		t.Fatalf("ensure did not start both: cmds=%v spawned=%v", f.cmds, *sp)
	}
	// 两者都在运行：什么都不做
	_ = os.MkdirAll(f.path(l.RunDir()), 0o755)
	_ = os.WriteFile(f.path(l.PidFile()), []byte("4242\n"), 0o644)
	fakeProc := func(pid, exe string) {
		_ = os.RemoveAll(f.path("/proc/" + pid))
		_ = os.MkdirAll(f.path("/proc/"+pid), 0o755)
		if err := os.Symlink(exe, f.path("/proc/"+pid+"/exe")); err != nil {
			t.Fatal(err)
		}
	}
	fakeProc("4242", l.Bin())
	fakeProc("777", l.AgentBin())
	f.cmds, *sp = nil, nil
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 || len(*sp) != 0 {
		t.Fatalf("ensure touched running processes: %v %v", f.cmds, *sp)
	}
	// 热升级后 master 运行 versions/<版本>/ 下的二进制，agent 自升级后运行状态目录中的版本：仍是我们的进程
	fakeProc("4242", uroot+"/versions/9.1.0/sbin/syedge")
	fakeProc("777", l.AgentState()+"/agent/0.3.1/syedge-agent")
	f.cmds, *sp = nil, nil
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 || len(*sp) != 0 {
		t.Fatalf("upgraded processes treated as stopped: %v %v", f.cmds, *sp)
	}
	// 版本库之外、名字相同的程序不算
	fakeProc("4242", uroot+"/versions/9.1.0/other/syedge")
	if f.alive(l.PidFile(), dataplaneExe(l)) {
		t.Fatal("binary outside the versions layout accepted")
	}
	// 重启后 pid 被本用户的其他程序复用：不能当成数据面在运行
	fakeProc("4242", "/usr/bin/vim")
	f.cmds, *sp = nil, nil
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	if !f.ran(uroot+"/sbin/syedge -e") || len(*sp) != 0 {
		t.Fatalf("reused pid treated as running: cmds=%v spawned=%v", f.cmds, *sp)
	}
	// system 模式下不可用
	sys := newFake(t)
	if err := sys.Ensure(); err == nil {
		t.Fatal("ensure in system mode accepted")
	}
}

func TestUserRemove(t *testing.T) {
	f, _ := newUserFake(t, false)
	if err := f.Add(AddOptions{Profile: userPorts}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(AddOptions{Profile: render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8083}}); err != nil {
		t.Fatal(err)
	}
	b, _ := layout.User(uroot, "blue")
	_ = os.WriteFile(f.path(b.AccessLog()), []byte("x\n"), 0o644)
	// blue 已热升级：master 运行版本库中的二进制，删除时仍须让它优雅退出
	_ = os.MkdirAll(f.path(b.RunDir()), 0o755)
	_ = os.WriteFile(f.path(b.PidFile()), []byte("5150\n"), 0o644)
	_ = os.MkdirAll(f.path("/proc/5150"), 0o755)
	_ = os.Symlink(uroot+"/versions/9.1.0/sbin/syedge", f.path("/proc/5150/exe"))
	f.cmds = nil
	if err := f.Remove("blue", false, true); err != nil {
		t.Fatal(err)
	}
	if !f.ran(uroot + "/sbin/syedge -e " + b.ErrorLog() + " -c " + b.CurrentConf() + " -s quit") {
		t.Fatalf("upgraded master not asked to quit: %v", f.cmds)
	}
	if _, err := os.Stat(f.path(b.Prefix())); err == nil {
		t.Fatal("prefix not removed")
	}
	moved, _ := filepath.Glob(f.path(uroot + "/removed-logs/blue-*/access.json"))
	if len(moved) != 1 {
		t.Fatal("logs not kept without --purge")
	}
	d, _ := layout.User(uroot, "")
	if _, err := os.Stat(f.path(d.InstanceFile())); err != nil {
		t.Fatal("default instance affected")
	}
	if err := f.Remove("", true, true); err == nil {
		t.Fatal("removing the default user instance accepted")
	}
	list, _ := f.List()
	if len(list) != 1 || list[0].Name != "" {
		t.Fatalf("list after remove = %+v", list)
	}
}
