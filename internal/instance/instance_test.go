package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/wrapper"
)

type fakeEnv struct {
	Env
	cmds   []string
	failOn string   // 执行到包含此子串的命令时失败
	caps   []string // SetCaps 写入能力的文件
}

func newFake(t *testing.T) *fakeEnv {
	t.Helper()
	f := &fakeEnv{}
	f.Env = Env{
		Root: t.TempDir(),
		Out:  &bytes.Buffer{},
		Run: func(name string, args ...string) error {
			c := strings.Join(append([]string{name}, args...), " ")
			f.cmds = append(f.cmds, c)
			if f.failOn != "" && strings.Contains(c, f.failOn) {
				return os.ErrPermission
			}
			// 模拟真实 bootstrap 的结果：写出该实例的当前配置
			if len(args) >= 2 && args[len(args)-2] == "-instance" && strings.Contains(c, " bootstrap ") {
				conf := f.path(layout.System(args[len(args)-1]).CurrentConf())
				_ = os.MkdirAll(filepath.Dir(conf), 0o755)
				_ = os.WriteFile(conf, []byte("# baseline\n"), 0o644)
			}
			return nil
		},
		Listening: func() (map[string]bool, error) { return map[string]bool{}, nil },
		SetCaps: func(path string, value []byte) error {
			f.caps = append(f.caps, path)
			return nil
		},
	}
	return f
}

func (f *fakeEnv) ran(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

var blue = render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}

func TestAddCreatesInstance(t *testing.T) {
	f := newFake(t)
	err := f.Add(AddOptions{Profile: blue, Console: "console.example.com:9443", Token: "tok", CAFingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	l := layout.System("blue")
	// 实例档案：root 所有、他人不可写，内容为档案
	st, err := os.Stat(f.path(l.InstanceFile()))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("instance.json: %v %v", st, err)
	}
	b, _ := os.ReadFile(f.path(l.InstanceFile()))
	var got render.Profile
	if err := json.Unmarshal(b, &got); err != nil || got != blue {
		t.Fatalf("instance.json = %s", b)
	}
	if st, _ := os.Stat(f.path(l.Prefix())); st == nil || st.Mode().Perm() != 0o755 {
		t.Fatal("prefix dir must be 0755")
	}
	for _, d := range []string{l.ReleasesDir(), l.LogDir(), l.CacheDir(), l.AgentState(), l.RunDir()} {
		if _, err := os.Stat(f.path(d)); err != nil {
			t.Errorf("missing dir %s", d)
		}
	}
	// 每实例 sudoers：7 行、实例名写死、0440
	sb, err := os.ReadFile(f.path(l.SudoersFile()))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(f.path(l.SudoersFile())); st.Mode().Perm() != 0o440 {
		t.Fatalf("sudoers mode %v", st.Mode())
	}
	var rules []string
	for _, line := range strings.Split(string(sb), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}
	if len(rules) != 7 {
		t.Fatalf("sudoers rules = %d:\n%s", len(rules), sb)
	}
	for _, r := range rules {
		if !strings.HasPrefix(r, "syapp-agent-blue ALL=(syapp) NOPASSWD: /usr/libexec/syapp/syapp-wrapper --instance blue ") {
			t.Errorf("rule not pinned to the instance: %q", r)
		}
	}
	if strings.Count(string(sb), "*") != 2 {
		t.Errorf("only test and upgrade-start may take a wildcard:\n%s", sb)
	}
	// 命令顺序：建用户 → visudo 校验 → 初始版本 → syapp -t → 启动数据面 → 注册 → 启动 agent
	want := []string{
		"useradd -r -g syapp -d /nonexistent -s /usr/sbin/nologin syapp-agent-blue",
		"visudo -cf ",
		"runuser -u syapp-agent-blue -- /usr/bin/syapp-agent bootstrap -instance blue",
		"runuser -u syapp -- /usr/libexec/syapp/syapp-wrapper --instance blue test r000000",
		"systemctl enable --now syapp@blue.service",
		"runuser -u syapp-agent-blue -- /usr/bin/syapp-agent enroll -instance blue -server console.example.com:9443 -token tok -ca-fingerprint abc",
		"systemctl enable --now syapp-agent@blue.service",
	}
	i := 0
	for _, c := range f.cmds {
		if i < len(want) && strings.HasPrefix(c, want[i]) {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("commands out of order or missing (matched %d of %d):\n%s", i, len(want), strings.Join(f.cmds, "\n"))
	}
	// 再次添加同名实例：拒绝
	if err := f.Add(AddOptions{Profile: blue}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate add: %v", err)
	}
}

func TestAddWithoutConsoleDoesNotStartAgent(t *testing.T) {
	f := newFake(t)
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	if f.ran("enroll") || f.ran("syapp-agent@blue.service") {
		t.Fatalf("agent started without enrollment:\n%s", strings.Join(f.cmds, "\n"))
	}
	if !strings.Contains(f.Out.(*bytes.Buffer).String(), "-instance blue") {
		t.Error("output must tell the operator how to enroll later")
	}
}

func TestAddRejectsConflicts(t *testing.T) {
	f := newFake(t)
	// 默认实例已安装（80 / 443 / 8081）
	if err := os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]render.Profile{
		"https clashes with default": {Instance: "green", HTTPPort: 28080, HTTPSPort: 443, StatusPort: 8083},
		"status clashes with blue":   {Instance: "green", HTTPPort: 28080, HTTPSPort: 28443, StatusPort: 8082},
		"default instance via add":   {},
		"invalid profile":            {Instance: "green", HTTPPort: 28080, HTTPSPort: 28080},
	} {
		if err := f.Add(AddOptions{Profile: p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 端口已被其他程序监听
	f.Listening = func() (map[string]bool, error) { return map[string]bool{"tcp/28443": true}, nil }
	if err := f.Add(AddOptions{Profile: render.Profile{Instance: "green", HTTPPort: 28080, HTTPSPort: 28443, StatusPort: 8083}}); err == nil || !strings.Contains(err.Error(), "28443") {
		t.Fatalf("listening port: %v", err)
	}
}

func TestAddFailureTellsHowToCleanUp(t *testing.T) {
	f := newFake(t)
	f.failOn = "visudo"
	err := f.Add(AddOptions{Profile: blue})
	if err == nil || !strings.Contains(err.Error(), "remove blue") {
		t.Fatalf("want cleanup hint, got %v", err)
	}
	// visudo 校验失败时不留下 sudoers 文件
	if _, err := os.Stat(f.path(layout.System("blue").SudoersFile())); err == nil {
		t.Fatal("unchecked sudoers file left in place")
	}
	if f.ran("systemctl enable") {
		t.Fatal("services started after a failed step")
	}
}

func TestRemove(t *testing.T) {
	f := newFake(t)
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	green := render.Profile{Instance: "green", HTTPPort: 28080, HTTPSPort: 28443, StatusPort: 8083}
	if err := f.Add(AddOptions{Profile: green}); err != nil {
		t.Fatal(err)
	}
	l := layout.System("blue")
	// 不带 --yes：只打印计划，不做任何事
	f.cmds = nil
	if err := f.Remove("blue", false, false); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("dry run executed commands: %v", f.cmds)
	}
	if _, err := os.Stat(f.path(l.InstanceFile())); err != nil {
		t.Fatal("dry run removed files")
	}
	if err := f.Remove("blue", false, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"systemctl disable --now syapp-agent@blue.service", "systemctl disable --now syapp@blue.service", "userdel syapp-agent-blue"} {
		if !f.ran(want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, gone := range []string{l.Prefix(), l.AgentState(), l.SudoersFile()} {
		if _, err := os.Stat(f.path(gone)); err == nil {
			t.Errorf("%s not removed", gone)
		}
	}
	// 不加 --purge 保留日志
	if _, err := os.Stat(f.path(l.LogDir())); err != nil {
		t.Error("logs removed without --purge")
	}
	// 其他实例不受影响
	g := layout.System("green")
	for _, keep := range []string{g.InstanceFile(), g.SudoersFile(), g.ReleasesDir()} {
		if _, err := os.Stat(f.path(keep)); err != nil {
			t.Errorf("green lost %s", keep)
		}
	}
	if f.ran("syapp@green") || f.ran("syapp-agent-green") {
		t.Fatal("removing blue touched green")
	}
	// --purge 同时删日志与缓存
	if err := f.Remove("green", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path(g.LogDir())); err == nil {
		t.Error("--purge kept logs")
	}
	// 不存在的实例、默认实例、非法名称
	for _, n := range []string{"nope", "", "../etc"} {
		if err := f.Remove(n, true, true); err == nil {
			t.Errorf("remove %q accepted", n)
		}
	}
}

func TestListAndCheck(t *testing.T) {
	f := newFake(t)
	if err := os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	// 运行中的 master：pid 文件指向存在的进程
	l := layout.System("blue")
	if err := os.MkdirAll(f.path(l.RunDir()), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(f.path(l.PidFile()), []byte("4242\n"), 0o644)
	_ = os.MkdirAll(f.path("/proc/4242"), 0o755)
	list, err := f.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "" || list[1].Name != "blue" || !list[1].Running || list[0].Running {
		t.Fatalf("list = %+v", list)
	}
	if probs := f.Check(); len(probs) != 0 {
		t.Fatalf("healthy host reported problems: %v", probs)
	}
	// 手工改坏：sudoers 被删除、另一个实例档案与 blue 端口冲突
	_ = os.Remove(f.path(l.SudoersFile()))
	bad := layout.System("red")
	_ = os.MkdirAll(f.path(bad.Prefix()), 0o755)
	b, _ := json.Marshal(render.Profile{Instance: "red", HTTPPort: 18080, HTTPSPort: 38443, StatusPort: 8090})
	_ = os.WriteFile(f.path(bad.InstanceFile()), b, 0o644)
	probs := strings.Join(f.Check(), "\n")
	for _, want := range []string{"sudoers", "tcp/18080"} {
		if !strings.Contains(probs, want) {
			t.Errorf("check did not report %q:\n%s", want, probs)
		}
	}
}

func TestListeningFromProc(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "proc/net"), 0o755)
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0A000001:C350 0A000002:0050 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
`
	_ = os.WriteFile(filepath.Join(root, "proc/net/tcp"), []byte(tcp), 0o644)
	udp := `  sl  local_address rem_address   st
   0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 4 2 0000000000000000 0
`
	_ = os.WriteFile(filepath.Join(root, "proc/net/udp"), []byte(udp), 0o644)
	got, err := ListeningFromProc(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tcp/80", "tcp/8081", "udp/53"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if got["tcp/50000"] {
		t.Error("established (non-listening) socket counted")
	}
}

// 生成的 sudoers 必须能通过真实的 visudo 语法检查（没有 visudo 的环境跳过）。
func TestSudoersPassesVisudo(t *testing.T) {
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo not installed")
	}
	f := filepath.Join(t.TempDir(), "sudoers")
	if err := os.WriteFile(f, []byte(Sudoers(layout.System("blue"))), 0o440); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(visudo, "-cf", f).CombinedOutput(); err != nil {
		t.Fatalf("visudo rejected generated sudoers: %v\n%s", err, out)
	}
}

func TestReopenAll(t *testing.T) {
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	if err := f.ReopenAll(); err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/syapp/sbin/syapp -s reopen", "/opt/syapp/sbin/syapp -c /opt/syapp/instances/blue/conf/current/syapp.conf -s reopen"}
	// 运行时不使用 root（D5）：以 root 执行时经 runuser 以数据面用户发送信号，`-s` 打开的日志不会变成 root 所有
	if os.Geteuid() == 0 {
		for i := range want {
			want[i] = "runuser -u syapp -- " + want[i]
		}
	}
	if strings.Join(f.cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q", f.cmds)
	}
}

// 实例增删后：防火墙 path 单元监视各具名实例的配置目录，并刷新规则（防火墙已安装时）。
func TestAddRemoveSyncsFirewall(t *testing.T) {
	f := newFake(t)
	fw := f.path("/usr/sbin/syapp-firewall")
	_ = os.MkdirAll(filepath.Dir(fw), 0o755)
	_ = os.WriteFile(fw, []byte("#!/bin/sh\n"), 0o755)
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	green := render.Profile{Instance: "green", HTTPPort: 28080, HTTPSPort: 28443, StatusPort: 8083}
	if err := f.Add(AddOptions{Profile: green}); err != nil {
		t.Fatal(err)
	}
	dropIn := f.path("/etc/systemd/system/syapp-firewall-refresh.path.d/instances.conf")
	b, _ := os.ReadFile(dropIn)
	for _, want := range []string{"PathChanged=/opt/syapp/instances/blue/conf\n", "PathChanged=/opt/syapp/instances/green/conf\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("drop-in missing %q:\n%s", want, b)
		}
	}
	if !f.ran("systemctl start syapp-firewall-refresh.service") || !f.ran("systemctl try-restart syapp-firewall-refresh.path") {
		t.Fatalf("firewall not refreshed: %v", f.cmds)
	}
	f.cmds = nil
	if err := f.Remove("blue", false, true); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(dropIn)
	if strings.Contains(string(b), "/blue/") || !strings.Contains(string(b), "/green/") {
		t.Fatalf("drop-in after remove:\n%s", b)
	}
	if !f.ran("systemctl start syapp-firewall-refresh.service") {
		t.Fatal("firewall not refreshed after remove")
	}
	// 刷新失败只提示，不影响实例操作
	f.failOn = "start syapp-firewall-refresh.service"
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatalf("firewall failure broke add: %v", err)
	}
}

// D5：具名实例的运行时目录、日志与缓存属于数据面用户；nofile 超过单元默认值时写单元附加配置。
func TestAddNonRootRuntime(t *testing.T) {
	f := newFake(t)
	p := blue
	p.NoFile = 200000
	if err := f.Add(AddOptions{Profile: p}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chown syapp:syapp /var/log/syapp-blue", "chown syapp:syapp /var/cache/syapp-blue", "chown syapp:syapp /run/syapp-blue",
		"runuser -u syapp -- /usr/libexec/syapp/syapp-wrapper --instance blue test r000000"} {
		if !f.ran(want) {
			t.Errorf("missing %q in %v", want, f.cmds)
		}
	}
	b, err := os.ReadFile(f.path("/etc/systemd/system/syapp@blue.service.d/nofile.conf"))
	if err != nil || !strings.Contains(string(b), "LimitNOFILE=200000") {
		t.Fatalf("nofile drop-in: %q %v", b, err)
	}
	if err := f.Remove("blue", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("/etc/systemd/system/syapp@blue.service.d")); err == nil {
		t.Fatal("drop-in left after remove")
	}
}

// D5：nonroot 交出运行时目录、重写具名实例 sudoers、切换仍以 root 运行的 master、重启 agent；可重复执行。
func TestNonroot(t *testing.T) {
	f := newFake(t)
	if err := f.Add(AddOptions{Profile: blue}); err != nil {
		t.Fatal(err)
	}
	def := layout.System("")
	_ = os.MkdirAll(f.path(def.ConfRoot()), 0o755) // 默认实例随安装包创建
	for _, l := range []layout.Layout{def, layout.System("blue")} {
		_ = os.MkdirAll(f.path(l.RunDir()), 0o755)
		_ = os.WriteFile(f.path(l.PidFile()), []byte("100\n"), 0o644)
	}
	w := f.path("/usr/libexec/syapp/syapp-wrapper")
	_ = os.MkdirAll(filepath.Dir(w), 0o755)
	_ = os.WriteFile(w, []byte("x"), 0o755)
	// 升级前生成的 sudoers：以 root 运行 wrapper
	_ = os.WriteFile(f.path(layout.System("blue").SudoersFile()), []byte("syapp-agent-blue ALL=(root) NOPASSWD: x\n"), 0o440)
	var handed []string
	f.Handover = func(name string) error {
		handed = append(handed, name)
		if name == "blue" {
			return wrapper.ErrNotRoot
		}
		return nil
	}
	f.cmds = nil
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(handed) != "[ blue]" {
		t.Fatalf("handover calls %q", handed)
	}
	for _, want := range []string{"chown -R syapp:syapp /opt/syapp/versions", "chown syapp:syapp /opt/syapp/sbin", "chown syapp:syapp /opt/syapp/modules",
		"chown -R syapp:syapp /run/syapp", "chown -R syapp:syapp /var/log/syapp", "chown -R syapp:syapp /var/cache/syapp-blue",
		"systemctl try-restart syapp-agent.service syapp-agent@blue.service"} {
		if !f.ran(want) {
			t.Errorf("missing %q in %v", want, f.cmds)
		}
	}
	// 版本库在切换之后再交一次（切换以 root 执行，会留下 root 所有的升级锁与首次收入的原二进制）
	last := -1
	for i, c := range f.cmds {
		if c == "chown -R syapp:syapp /opt/syapp/versions" {
			last = i
		}
	}
	if last < len(f.cmds)-2 {
		t.Fatalf("versions not re-owned after the handover: %v", f.cmds)
	}
	for _, want := range []string{} {
		if !f.ran(want) {
			t.Errorf("missing %q in %v", want, f.cmds)
		}
	}
	// wrapper 带 CAP_NET_BIND_SERVICE 文件能力，只允许数据面组执行
	if len(f.caps) == 0 || f.caps[len(f.caps)-1] != w || !f.ran("chmod 0750 /usr/libexec/syapp/syapp-wrapper") || !f.ran("chown root:syapp /usr/libexec/syapp/syapp-wrapper") {
		t.Fatalf("wrapper capability not set: caps=%v cmds=%v", f.caps, f.cmds)
	}
	b, _ := os.ReadFile(f.path(layout.System("blue").SudoersFile()))
	if strings.Contains(string(b), "(root)") || !strings.Contains(string(b), "ALL=(syapp) NOPASSWD:") {
		t.Fatalf("sudoers not rewritten:\n%s", b)
	}
	// 切换失败：返回错误（数据面一直在服务）
	f.Handover = func(string) error { return errors.New("boom") }
	if err := f.Nonroot(); err == nil || !strings.Contains(err.Error(), "仍以 root 运行") {
		t.Fatalf("failed handover: %v", err)
	}
	// user 模式不需要
	u, _ := newUserFake(t, false)
	if err := u.Nonroot(); err == nil {
		t.Fatal("nonroot accepted in user mode")
	}
}
