package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// user 模式（设计 0001 第 6 节）：普通用户在自己的安装根目录下运行全部实例，不需要 root，也不经过 sudo。
// 与 system 模式的区别：不建用户、不写 sudoers、不 chown；agent 与 wrapper 直接以当前用户运行；
// 开机自启优先用 systemd --user，不可用时由 ensure（cron 调用）拉起，ensure 只启动未运行的进程，从不重启正在运行的数据面。

func (e Env) user() bool { return e.UserRoot != "" }

// lay 返回当前模式下实例的布局。
func (e Env) lay(name string) layout.Layout {
	if e.user() {
		l, err := layout.User(e.UserRoot, name)
		if err != nil {
			panic("instance: invalid user root or instance name: " + err.Error()) // 调用方已校验
		}
		return l
	}
	return layout.SystemAt(e.systemRoot(), name)
}

// infoLayout 按已读取的实例档案确定该实例的数据面运行目录。
func (e Env) infoLayout(in Info) layout.Layout {
	l := e.lay(in.Name)
	if in.Profile.ResourceMode == "independent" {
		l = l.WithIndependentRuntime()
	}
	return l
}

// agentArgs 返回 agent 在 user 模式下的公共参数。
func agentArgs(l layout.Layout) []string {
	a := []string{"-root", l.Root()}
	if l.Instance() != "" {
		a = append(a, "-instance", l.Instance())
	}
	return a
}

// userUnits 返回 systemd --user 单元名（不用模板单元：单元里要写死本安装根目录的绝对路径）。
func userUnits(l layout.Layout) (dataplane, agent string) {
	sfx := ""
	if l.Instance() != "" {
		sfx = "-" + l.Instance()
	}
	return brand.ID() + sfx + ".service", brand.Agent() + sfx + ".service"
}

func (e Env) unitDir() string { return filepath.Join(e.Home, ".config", "systemd", "user") }

func (e Env) addUser(o AddOptions) error {
	if os.Geteuid() == 0 && e.Root == "/" {
		return errors.New("user 模式（--root）请以普通用户运行；以 root 安装请使用软件包（system 模式）")
	}
	if _, err := layout.User(e.UserRoot, o.Profile.Instance); err != nil {
		return err
	}
	p := o.Profile
	p.Mode, p.Root = string(layout.ModeUser), e.UserRoot
	if err := p.Validate(); err != nil {
		return err // 含：user 模式只能监听 1024 及以上的端口
	}
	enroll := o.Console != "" || o.Token != "" || o.CAFingerprint != ""
	if enroll && (o.Console == "" || o.Token == "" || o.CAFingerprint == "") {
		return errors.New("--console、--token、--ca-fingerprint 需同时提供")
	}
	l := e.lay(p.Instance)
	if p.ResourceMode == "independent" {
		l = l.WithIndependentRuntime()
	}
	if err := e.prepareInstancesDir(l); err != nil {
		return err
	}
	if _, err := os.Stat(e.path(l.InstanceFile())); err == nil {
		return fmt.Errorf("%w: %s（%s）", ErrExists, label(p.Instance), l.Prefix())
	}
	installed, err := e.installed()
	if err != nil {
		return err
	}
	if probs := conflicts(append(installed, Info{Name: p.Instance, Profile: p})); len(probs) > 0 {
		return fmt.Errorf("端口或实例名冲突：%s", strings.Join(probs, "；"))
	}
	if e.Listening != nil {
		busy, err := e.Listening()
		if err != nil {
			return err
		}
		for _, k := range sortedKeys(ports(p)) {
			if busy[k] {
				return fmt.Errorf("端口 %s 已被本机其他程序监听", k)
			}
		}
	}
	if err := e.addUserSteps(AddOptions{Profile: p, Console: o.Console, Token: o.Token, CAFingerprint: o.CAFingerprint}, l); err != nil {
		hint := "remove " + p.Instance + " --yes"
		if p.Instance == "" {
			hint = "删除 " + l.Prefix() + " 下的 instance.json"
		}
		return fmt.Errorf("%w（实例未完整创建，可执行 %s-instance --root %s %s 清理后重试）", err, brand.ID(), e.UserRoot, hint)
	}
	return nil
}

func (e Env) addUserSteps(o AddOptions, l layout.Layout) error {
	e.say("== 创建 %s（user 模式，%s）", label(l.Instance()), l.Prefix())
	for _, d := range []struct {
		dir  string
		mode os.FileMode
	}{
		{l.Prefix(), 0o755}, {l.ConfRoot(), 0o755}, {l.ReleasesDir(), 0o755},
		{l.LogDir(), 0o755}, {l.CacheDir(), 0o755}, {l.RunDir(), 0o755}, {l.AgentState(), 0o700},
	} {
		if err := os.MkdirAll(e.path(d.dir), d.mode); err != nil {
			return err
		}
		if err := os.Chmod(e.path(d.dir), d.mode); err != nil {
			return err
		}
	}
	if err := e.prepareIndependentRuntime(l); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(o.Profile, "", "  ")
	if err := writeFile(e.path(l.InstanceFile()), append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := e.Run(l.AgentBin(), append([]string{"bootstrap"}, agentArgs(l)...)...); err != nil {
		return fmt.Errorf("生成初始配置: %w", err)
	}
	if err := e.Run(l.Bin(), "-p", l.Prefix(), "-t", "-q"); err != nil {
		return fmt.Errorf("初始配置校验失败: %w", err)
	}
	systemd := e.UserSystemd != nil && e.UserSystemd()
	dpUnit, agUnit := userUnits(l)
	if systemd {
		if err := e.writeUserUnits(l); err != nil {
			return err
		}
		if err := e.Run("systemctl", "--user", "enable", "--now", dpUnit); err != nil {
			return fmt.Errorf("启动 %s: %w", dpUnit, err)
		}
	} else if err := e.startDataplane(l); err != nil {
		return err
	}
	p := o.Profile
	e.say("数据面已启动：HTTP %d / HTTPS %d / 状态页 127.0.0.1:%d", p.HTTP(), p.HTTPS(), p.Status())
	if o.Console != "" {
		args := append(append([]string{"enroll"}, agentArgs(l)...), "-server", o.Console, "-token", o.Token, "-ca-fingerprint", o.CAFingerprint)
		if err := e.Run(l.AgentBin(), args...); err != nil {
			return fmt.Errorf("注册到控制台: %w", err)
		}
		if systemd {
			if err := e.Run("systemctl", "--user", "enable", "--now", agUnit); err != nil {
				return fmt.Errorf("启动 %s: %w", agUnit, err)
			}
		} else if err := e.startAgent(l); err != nil {
			return err
		}
		e.say("agent 已启动，几秒后节点会出现在控制台。")
	} else {
		e.say("下一步：在控制台为实例档案匹配的节点组生成注册令牌，然后执行：")
		e.say("  %s enroll %s -server <控制台> -token <令牌> -ca-fingerprint <指纹>", l.AgentBin(), strings.Join(agentArgs(l), " "))
	}
	e.saySupervision(systemd)
	return nil
}

// saySupervision 说明开机自启的方式。
func (e Env) saySupervision(systemd bool) {
	tool := filepath.Join(e.UserRoot, "bin", brand.ID()+"-instance")
	if systemd {
		e.say("已安装 systemd --user 单元。要在未登录时也保持运行与开机自启，需要开启 linger：loginctl enable-linger %s", os.Getenv("USER"))
		e.say("（若提示没有权限，请联系管理员执行一次，或改用下面的 cron 方式）")
		return
	}
	e.say("本机没有可用的 systemd --user，请在 crontab -e 中加入以下两行，实现开机自启与进程守护：")
	e.say("  @reboot %s --root %s ensure", tool, e.UserRoot)
	e.say("  * * * * * %s --root %s ensure >/dev/null 2>&1", tool, e.UserRoot)
}

// writeUserUnits 为该实例写入 systemd --user 单元。
func (e Env) writeUserUnits(l layout.Layout) error {
	dpUnit, agUnit := userUnits(l)
	dp := fmt.Sprintf(`[Unit]
Description=%[1]s data plane (%[2]s, user mode, %[3]s)
After=network-online.target

[Service]
Type=forking
PIDFile=%[4]s
ExecStartPre=%[5]s -p %[6]s -t -q
ExecStart=%[5]s -p %[6]s
ExecReload=%[5]s -p %[6]s -s reload
ExecStop=%[5]s -p %[6]s -s quit
KillMode=mixed
KillSignal=SIGQUIT

[Install]
WantedBy=default.target
`, brand.Product(), label(l.Instance()), l.Root(), l.PidFile(), l.Bin(), l.Prefix())
	ag := fmt.Sprintf(`[Unit]
Description=%[1]s agent (%[2]s, user mode, %[3]s)
After=network-online.target %[4]s

[Service]
ExecStart=%[5]s run %[6]s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, brand.Product(), label(l.Instance()), l.Root(), dpUnit, l.AgentBin(), strings.Join(agentArgs(l), " "))
	dir := e.path(e.unitDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, dpUnit), []byte(dp), 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, agUnit), []byte(ag), 0o644); err != nil {
		return err
	}
	return e.Run("systemctl", "--user", "daemon-reload")
}

func (e Env) agentPidFile(l layout.Layout) string { return filepath.Join(l.RunDir(), "agent.pid") }

// startDataplane 直接启动数据面（master 自行转入后台）。
func (e Env) startDataplane(l layout.Layout) error {
	if err := e.Run(l.Bin(), "-p", l.Prefix()); err != nil {
		return fmt.Errorf("启动数据面: %w", err)
	}
	return nil
}

// startAgent 在后台启动 agent，并记录 pid 供 ensure 判断。
func (e Env) startAgent(l layout.Layout) error {
	if e.Spawn == nil {
		return errors.New("no process launcher")
	}
	args := append([]string{"run"}, agentArgs(l)...)
	if err := e.Spawn(e.path(e.agentPidFile(l)), e.path(filepath.Join(l.LogDir(), "agent.log")), l.AgentBin(), args...); err != nil {
		return fmt.Errorf("启动 agent: %w", err)
	}
	return nil
}

// running 读取 pid 文件，返回仍在运行、且可执行文件属于我们的进程号（0 表示没有）。
// 核对可执行文件：重启后 pid 可能被本用户的其他进程复用，不能把它当成我们的进程，更不能给它发信号。
// ours 判断可执行文件：热升级后数据面 master 运行的是 versions/<版本>/ 下的二进制，agent 自升级后运行的是状态目录中的版本。
func (e Env) running(pidFile string, ours func(exe string) bool) int {
	b, err := os.ReadFile(e.path(pidFile))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	got, err := os.Readlink(e.path("/proc/" + strconv.Itoa(pid) + "/exe"))
	if err != nil || !ours(strings.TrimSuffix(got, " (deleted)")) {
		return 0
	}
	return pid
}

// alive 判断 pid 文件中的进程是否仍在运行我们的程序。
func (e Env) alive(pidFile string, ours func(string) bool) bool { return e.running(pidFile, ours) != 0 }

// dataplaneExe：安装的二进制，或版本库中任一版本的二进制（热升级后）。
func dataplaneExe(l layout.Layout) func(string) bool {
	return func(exe string) bool {
		if exe == l.Bin() {
			return true
		}
		rel, err := filepath.Rel(l.Versions(), exe)
		parts := strings.Split(rel, "/")
		return err == nil && len(parts) == 3 && parts[0] != ".." && parts[1] == "sbin" && parts[2] == brand.ID()
	}
}

// agentExe：安装的 agent，或该实例状态目录中自升级安装的版本（agent/<版本>/<品牌>-agent）。
func agentExe(l layout.Layout) func(string) bool {
	return func(exe string) bool {
		if exe == l.AgentBin() {
			return true
		}
		rel, err := filepath.Rel(filepath.Join(l.AgentState(), "agent"), exe)
		parts := strings.Split(rel, "/")
		return err == nil && len(parts) == 2 && parts[0] != ".." && parts[1] == brand.Agent()
	}
}

// Ensure 启动 user 模式下未在运行的数据面与（已注册的）agent；正在运行的进程不受影响。供 cron 每分钟调用。
func (e Env) Ensure() error {
	if !e.user() {
		return errors.New("ensure 只用于 user 模式（--root）；system 模式由 systemd 守护")
	}
	list, err := e.installed()
	if err != nil {
		return err
	}
	systemd := e.UserSystemd != nil && e.UserSystemd()
	var errs []error
	for _, in := range list {
		l := e.infoLayout(in)
		dpUnit, agUnit := userUnits(l)
		if !e.alive(l.PidFile(), dataplaneExe(l)) {
			if systemd {
				errs = append(errs, e.Run("systemctl", "--user", "start", dpUnit))
			} else {
				errs = append(errs, e.startDataplane(l))
			}
			e.say("%s：已启动数据面", label(in.Name))
		}
		if _, err := os.Stat(e.path(filepath.Join(l.AgentState(), "node.json"))); err != nil {
			continue // 尚未注册
		}
		if systemd {
			errs = append(errs, e.Run("systemctl", "--user", "start", agUnit)) // 已在运行时为空操作
		} else if !e.alive(e.agentPidFile(l), agentExe(l)) {
			errs = append(errs, e.startAgent(l))
			e.say("%s：已启动 agent", label(in.Name))
		}
	}
	return errors.Join(errs...)
}

func (e Env) removeUser(name string, purge, yes bool) error {
	if name == "" || !layout.ValidInstance(name) {
		return errors.New("remove 只用于具名实例；默认实例随安装根目录一起删除")
	}
	l := e.lay(name)
	if _, err := os.Stat(e.path(l.Prefix())); err != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err := e.prepareInstancesDir(l); err != nil {
		return err
	}
	if b, err := os.ReadFile(e.path(l.InstanceFile())); err == nil {
		var p render.Profile
		if err := json.Unmarshal(b, &p); err != nil {
			return fmt.Errorf("invalid instance profile: %w", err)
		}
		if p.Instance != name || p.Mode != string(layout.ModeUser) || p.Root != e.UserRoot {
			return errors.New("instance profile does not match the user installation")
		}
		l = e.infoLayout(Info{Name: name, Profile: p})
	} else if !os.IsNotExist(err) {
		return err
	}
	dpUnit, agUnit := userUnits(l)
	keep := filepath.Join(e.UserRoot, "removed-logs", name+"-"+time.Now().Format("20060102-150405"))
	if !yes {
		e.say("将删除实例 %s（加 --yes 执行）：", name)
		e.say("  停止 agent，数据面优雅退出（现有连接处理完再结束）")
		e.say("  删除目录 %s", l.Prefix())
		if !purge {
			e.say("  日志移到 %s（加 --purge 一并删除）", keep)
		}
		return nil
	}
	// 先停 agent，再让数据面优雅退出；单元或进程不存在不影响后续清理
	hasUnit := func(u string) bool {
		_, err := os.Stat(filepath.Join(e.path(e.unitDir()), u))
		return err == nil
	}
	if hasUnit(agUnit) {
		_ = e.Run("systemctl", "--user", "disable", "--now", agUnit)
	}
	if pid := e.running(e.agentPidFile(l), agentExe(l)); pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	if hasUnit(dpUnit) {
		_ = e.Run("systemctl", "--user", "disable", "--now", dpUnit)
	}
	if e.alive(l.PidFile(), dataplaneExe(l)) {
		_ = e.Run(l.Bin(), "-p", l.Prefix(), "-s", "quit")
	}
	for _, u := range []string{dpUnit, agUnit} {
		if err := os.Remove(filepath.Join(e.path(e.unitDir()), u)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if !purge {
		if _, err := os.Stat(e.path(l.LogDir())); err == nil {
			if err := os.MkdirAll(filepath.Dir(e.path(keep)), 0o755); err != nil {
				return err
			}
			if err := os.Rename(e.path(l.LogDir()), e.path(keep)); err != nil {
				return err
			}
		}
	}
	if err := removeInstanceDir(e.path(l.Prefix()), name); err != nil {
		return err
	}
	e.say("实例 %s 已删除。请在控制台“节点”页删除对应节点。", name)
	return nil
}
