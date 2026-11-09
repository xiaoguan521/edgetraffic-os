// Package instance 实现同机多实例的管理命令 <品牌>-instance（设计 0001 第 5.7 节）：
// add / remove / list / check。
//
// 文件系统根与外部命令可注入（Env），测试在临时目录中完整执行 add / remove 并核对发出的命令；
// 外部命令一律以参数切片执行，不经过 shell。所有路径由 internal/layout 按校验过的实例名计算。
package instance

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// Env 是命令运行的环境。
type Env struct {
	Root      string                                  // 文件系统根：生产为 "/"
	Run       func(name string, args ...string) error // 执行外部命令（参数切片，不经过 shell）
	Out       io.Writer                               // 面向运维人员的输出
	Listening func() (map[string]bool, error)         // 本机正在监听的端口（"tcp/80"、"udp/53"）

	// 以下只用于 user 模式（普通用户安装，设计 0001 第 6 节，见 usermode.go）
	UserRoot    string                                                    // 安装根目录；非空即为 user 模式
	Home        string                                                    // 当前用户的主目录（写 systemd --user 单元）
	UserSystemd func() bool                                               // systemd --user 是否可用
	Spawn       func(pidFile, logFile, name string, args ...string) error // 在后台启动进程（脱离终端）并写 pid 文件

	// Handover 把以 root 运行的实例 master 无中断地切换为数据面用户（nonroot，见 nonroot.go）
	Handover func(instance string) error
	// SetCaps 给文件写入 security.capability 扩展属性（wrapper 的 CAP_NET_BIND_SERVICE，见 nonroot.go）
	SetCaps func(path string, value []byte) error

	// Ambient 报告本机内核与 systemd 是否支持 ambient 能力（nil = 支持）；不支持时 nonroot 进入兼容模式（rootmaster.go）
	Ambient func() bool
	// RootMaster：nonroot 显式选择兼容模式（--root-master），与传统 Web 服务器相同由 root 运行 master
	RootMaster bool
}

func (e Env) path(p string) string { return filepath.Join(e.Root, filepath.FromSlash(p)) }

func (e Env) systemRoot() string {
	root, err := layout.LoadSystemRootAt(e.Root)
	if err != nil {
		panic("instance: invalid system root: " + err.Error())
	}
	return root
}

func (e Env) say(format string, a ...any) { _, _ = fmt.Fprintf(e.Out, format+"\n", a...) }

// AddOptions 是 add 的参数。
type AddOptions struct {
	Profile render.Profile
	// 三项同时提供时，add 完成后立即注册到控制台并启动 agent
	Console, Token, CAFingerprint string
}

// Info 是 list 的一行。
type Info struct {
	Name    string // 空串为默认实例
	Profile render.Profile
	Running bool // master 进程存在
	PID     int
}

var (
	// ErrExists 表示实例已存在。
	ErrExists = errors.New("instance already exists")
	// ErrNotFound 表示实例不存在。
	ErrNotFound = errors.New("instance not found")
)

// Add 创建并启动一个具名实例。
func (e Env) Add(o AddOptions) error {
	if e.user() {
		return e.addUser(o)
	}
	p := o.Profile
	if p.Instance == "" {
		return errors.New("add 只用于具名实例；默认实例由安装包创建")
	}
	if p.Mode != "" && p.Mode != string(layout.ModeSystem) {
		return errors.New("add 创建 system 模式的实例；user 模式请使用免安装包（设计 0001 第 6 节）")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	enroll := o.Console != "" || o.Token != "" || o.CAFingerprint != ""
	if enroll && (o.Console == "" || o.Token == "" || o.CAFingerprint == "") {
		return errors.New("--console、--token、--ca-fingerprint 需同时提供")
	}
	l := e.lay(p.Instance)
	if p.Root != "" && p.Root != l.Root() {
		return fmt.Errorf("实例档案安装根目录 %q 与本机 system 根目录 %q 不一致", p.Root, l.Root())
	}
	// system 模式的实例档案需要把自定义根目录写入节点，供 agent 与控制台核对；
	// 默认根目录继续使用零值，保持旧版 instance.json 兼容。
	if l.Root() != layout.DefaultSystemRoot() {
		p.Mode, p.Root = string(layout.ModeSystem), l.Root()
		if err := p.Validate(); err != nil {
			return err
		}
		o.Profile = p
	}
	if _, err := os.Stat(e.path(l.InstanceFile())); err == nil {
		return fmt.Errorf("%w: %s（%s）", ErrExists, p.Instance, l.Prefix())
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
	if err := e.add(o, l); err != nil {
		return fmt.Errorf("%w（实例未完整创建，可执行 %s remove %s --yes 清理后重试）", err, brand.ID()+"-instance", p.Instance)
	}
	return nil
}

func (e Env) add(o AddOptions, l layout.Layout) error {
	user, group := l.AgentUser(), layout.Group()
	e.say("== 创建实例 %s（%s）", l.Instance(), l.Prefix())
	if err := e.Run("useradd", "-r", "-g", group, "-d", "/nonexistent", "-s", "/usr/sbin/nologin", user); err != nil {
		return fmt.Errorf("创建用户 %s: %w", user, err)
	}
	// 目录：实例前缀与档案属于 root（wrapper 据此确认实例由 root 创建）；配置目录与 agent 状态目录属于该实例的 agent 用户；
	// 运行时目录、日志与缓存属于数据面用户（master 不以 root 运行，设计 0001 D5）
	dp := layout.WorkerUser()
	for _, d := range []struct {
		dir   string
		mode  os.FileMode
		owner string
	}{
		{l.Prefix(), 0o755, ""},
		{l.ConfRoot(), 0o755, user},
		{l.ReleasesDir(), 0o755, user},
		{l.LogDir(), 0o755, dp},
		{l.CacheDir(), 0o755, dp},
		{l.AgentState(), 0o750, user},
		// syedge -t 会检查 pid 文件所在目录；平时由服务单元的 RuntimeDirectory= 与 tmpfiles 创建，启动前先建好
		{l.RunDir(), 0o755, dp},
	} {
		if err := os.MkdirAll(e.path(d.dir), d.mode); err != nil {
			return err
		}
		if err := os.Chmod(e.path(d.dir), d.mode); err != nil {
			return err
		}
		if d.owner != "" {
			g := group
			if d.owner == dp {
				g = dp
			}
			if err := e.Run("chown", d.owner+":"+g, d.dir); err != nil {
				return err
			}
		}
	}
	b, _ := json.MarshalIndent(o.Profile, "", "  ")
	if err := writeFile(e.path(l.InstanceFile()), append(b, '\n'), 0o644); err != nil {
		return err
	}
	// sudoers：写入临时文件，visudo 校验通过才生效
	if err := e.writeSudoers(l); err != nil {
		return err
	}
	if err := e.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	// 初始版本由该实例的 agent 用户写入（与默认实例的安装流程一致），再用真实二进制校验
	if err := e.Run("runuser", "-u", user, "--", l.AgentBin(), "bootstrap", "-instance", l.Instance()); err != nil {
		return fmt.Errorf("生成初始配置: %w", err)
	}
	// 经 wrapper 以数据面用户校验（与运行时相同的身份与能力，不会以 root 身份创建日志文件）
	if err := e.wrapperCaps(); err != nil {
		return err
	}
	c := e.asWrapperUser(l.WrapperBin(), "--instance", l.Instance(), "test", "r000000")
	if err := e.Run(c[0], c[1:]...); err != nil {
		return fmt.Errorf("初始配置校验失败: %w", err)
	}
	if n := o.Profile.NoFile; n > defaultUnitNoFile {
		// 非 root 的 master 不能把打开文件数调到单元限制之上：按档案写单元附加配置
		dir := e.path("/etc/systemd/system/" + l.DataplaneUnit() + ".d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "nofile.conf"), []byte(fmt.Sprintf("[Service]\nLimitNOFILE=%d\n", n)), 0o644); err != nil {
			return err
		}
		if err := e.Run("systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	// 运行时目录登记到 tmpfiles：开机后 /run 被清空，systemd 低于 211（不支持 RuntimeDirectory=）时也能重建
	if err := e.writeRuntimeTmpfiles(); err != nil {
		return err
	}
	// enable 与 start 分开执行：systemctl enable --now 需要 systemd 220 以上（CentOS 7.0/7.1 为 208）
	if err := e.enableStart(l.DataplaneUnit()); err != nil {
		return fmt.Errorf("启动 %s: %w", l.DataplaneUnit(), err)
	}
	p := o.Profile
	e.say("%s 已启动：HTTP %d / HTTPS %d / 状态页 127.0.0.1:%d", l.DataplaneUnit(), p.HTTP(), p.HTTPS(), p.Status())
	e.syncFirewall()
	if o.Console == "" {
		e.say("下一步：在控制台为实例档案为 %s 的节点组生成注册令牌，然后执行：", l.Instance())
		e.say("  runuser -u %s -- %s enroll -instance %s -server <控制台> -token <令牌> -ca-fingerprint <指纹>", user, l.AgentBin(), l.Instance())
		e.say("  systemctl enable %s && systemctl start %s", l.AgentUnit(), l.AgentUnit())
		return nil
	}
	if err := e.Run("runuser", "-u", user, "--", l.AgentBin(), "enroll", "-instance", l.Instance(),
		"-server", o.Console, "-token", o.Token, "-ca-fingerprint", o.CAFingerprint); err != nil {
		return fmt.Errorf("注册到控制台: %w", err)
	}
	if err := e.enableStart(l.AgentUnit()); err != nil {
		return fmt.Errorf("启动 %s: %w", l.AgentUnit(), err)
	}
	e.say("%s 已启动，几秒后节点会出现在控制台。", l.AgentUnit())
	return nil
}

// Remove 删除一个具名实例。yes 为 false 时只打印将执行的操作。
func (e Env) Remove(name string, purge, yes bool) error {
	if e.user() {
		return e.removeUser(name, purge, yes)
	}
	if name == "" || !layout.ValidInstance(name) {
		return errors.New("remove 只用于具名实例，且实例名必须合法；默认实例随安装包卸载")
	}
	l := e.lay(name)
	if _, err := os.Stat(e.path(l.InstanceFile())); err != nil {
		if _, derr := os.Stat(e.path(l.Prefix())); derr != nil {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		// 半途失败的 add 只留下部分目录：仍允许清理
	}
	dirs := []string{l.Prefix(), l.AgentState()}
	if purge {
		dirs = append(dirs, l.LogDir(), l.CacheDir())
	}
	if !yes {
		e.say("将删除实例 %s（加 --yes 执行）：", name)
		e.say("  停止并停用 %s、%s（数据面优雅退出，现有连接处理完再结束）", l.AgentUnit(), l.DataplaneUnit())
		e.say("  删除 %s、用户 %s", l.SudoersFile(), l.AgentUser())
		e.say("  删除目录 %s", strings.Join(dirs, "、"))
		if !purge {
			e.say("  保留日志 %s 与缓存 %s（加 --purge 一并删除）", l.LogDir(), l.CacheDir())
		}
		return nil
	}
	// 先停 agent，再让数据面优雅退出；单元不存在等错误不影响后续清理
	for _, u := range []string{l.AgentUnit(), l.DataplaneUnit()} {
		_ = e.Run("systemctl", "stop", u)
		_ = e.Run("systemctl", "disable", u)
	}
	if err := os.Remove(e.path(l.SudoersFile())); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = e.Run("userdel", l.AgentUser())
	_ = os.RemoveAll(e.path("/etc/systemd/system/" + l.DataplaneUnit() + ".d"))
	for _, d := range dirs {
		if err := removeInstanceDir(e.path(d), name); err != nil {
			return err
		}
	}
	_ = e.Run("systemctl", "daemon-reload")
	_ = e.writeRuntimeTmpfiles()
	e.syncFirewall()
	e.say("实例 %s 已删除。请在控制台“节点”页吊销对应节点。", name)
	return nil
}

// enableStart 设为开机启动并立即启动（等同 systemctl enable --now，但兼容 systemd 220 以下）。
func (e Env) enableStart(unit string) error {
	if err := e.Run("systemctl", "enable", unit); err != nil {
		return err
	}
	return e.Run("systemctl", "start", unit)
}

// RuntimeTmpfiles 是 system 模式各实例运行时目录的 tmpfiles 配置（默认实例的数据面目录由软件包提供）。
func RuntimeTmpfiles() string { return "/etc/tmpfiles.d/" + brand.ID() + "-instances.conf" }

// writeRuntimeTmpfiles 按本机实例重写运行时目录的 tmpfiles 配置并立即创建目录：数据面的 pid 目录属于数据面用户，
// agent 的发布锁目录属于该实例的 agent 用户。服务单元的 RuntimeDirectory= 在 systemd 211 以下不生效，
// 而 /run 每次开机都会清空，因此这些目录交给 tmpfiles 在开机时重建。
func (e Env) writeRuntimeTmpfiles() error {
	if e.user() {
		return nil
	}
	insts, err := e.installed()
	if err != nil {
		return err
	}
	dp := layout.WorkerUser()
	var b strings.Builder
	b.WriteString("# 由 " + brand.ID() + "-instance 生成：各实例的运行时目录（开机时重建）。请勿手改\n")
	for _, in := range insts {
		l := e.lay(in.Name)
		if in.Name != "" {
			fmt.Fprintf(&b, "d %s 0755 %s %s -\n", l.RunDir(), dp, dp)
		}
		fmt.Fprintf(&b, "d %s 0755 %s %s -\n", filepath.Dir(l.AgentLock()), l.AgentUser(), layout.Group())
	}
	p := RuntimeTmpfiles()
	if err := os.MkdirAll(e.path(filepath.Dir(p)), 0o755); err != nil {
		return err
	}
	if err := writeFile(e.path(p), []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := e.Run("systemd-tmpfiles", "--create", p); err != nil {
		e.say("警告：systemd-tmpfiles --create %s 未完成：%v", p, err)
	}
	return nil
}

// FirewallRefreshUnit 是发布后自动刷新防火墙规则的 path 单元（设计 0001 第 5.8 节）。
func FirewallRefreshUnit() string { return brand.Firewall() + "-refresh.path" }

// firewallDropIn 是 path 单元的附加配置：列出具名实例的配置目录（默认实例的目录写在单元本身）。
func firewallDropIn() string {
	return "/etc/systemd/system/" + FirewallRefreshUnit() + ".d/instances.conf"
}

// syncFirewall 在实例增删后（system 模式）：让防火墙的 path 单元同时监视各实例的配置目录，
// 并按全部实例的当前配置刷新规则。防火墙未启用时 refresh 什么都不做；任何失败都只提示，不影响实例本身。
func (e Env) syncFirewall() {
	insts, err := e.installed()
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("# 由 " + brand.ID() + "-instance 生成：发布具名实例的配置后同样刷新防火墙规则\n[Path]\n")
	for _, in := range insts {
		if in.Name != "" {
			b.WriteString("PathChanged=" + e.lay(in.Name).ConfRoot() + "\n")
		}
	}
	dp := e.path(firewallDropIn())
	if err := os.MkdirAll(filepath.Dir(dp), 0o755); err == nil {
		_ = writeFile(dp, []byte(b.String()), 0o644)
	}
	_ = e.Run("systemctl", "daemon-reload")
	_ = e.Run("systemctl", "try-restart", FirewallRefreshUnit())
	if _, err := os.Stat(e.path("/usr/sbin/" + brand.Firewall())); err != nil {
		return
	}
	// 经刷新服务执行（以防火墙专用用户、只带 CAP_NET_ADMIN 运行，不以 root 运行）；未启用防火墙时服务的条件不满足，什么都不做
	if err := e.Run("systemctl", "start", FirewallRefreshService()); err != nil {
		e.say("警告：防火墙规则刷新失败（%v），现有规则保持不变；请查看 journalctl -u %s", err, FirewallRefreshService())
	}
}

// FirewallRefreshService 是刷新防火墙规则的服务。
func FirewallRefreshService() string { return brand.Firewall() + "-refresh.service" }

// removeInstanceDir 删除一个属于该实例的目录；路径必须带实例名，防止误删共用目录。
func removeInstanceDir(dir, name string) error {
	base := filepath.Base(dir)
	if base != name && !strings.HasSuffix(base, "-"+name) {
		return fmt.Errorf("refusing to remove %s: not an instance directory of %s", dir, name)
	}
	return os.RemoveAll(dir)
}

// installed 返回本机已安装的实例（默认实例在前，其余按名称排序）。
func (e Env) installed() ([]Info, error) {
	var out []Info
	def := e.lay("")
	marker := def.ConfRoot() // system 模式：默认实例随安装包创建；user 模式：默认实例也有档案
	if e.user() {
		marker = def.InstanceFile()
	}
	if _, err := os.Stat(e.path(marker)); err == nil {
		var p render.Profile // system 模式的默认实例没有档案，端口为默认值
		if e.user() {
			b, err := os.ReadFile(e.path(def.InstanceFile()))
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", def.InstanceFile(), err)
			}
		}
		out = append(out, Info{Name: "", Profile: p})
	}
	entries, err := os.ReadDir(e.path(def.InstancesDir()))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, de := range entries {
		name := de.Name()
		if !de.IsDir() || !layout.ValidInstance(name) || name == "" {
			continue
		}
		b, err := os.ReadFile(e.path(e.lay(name).InstanceFile()))
		if err != nil {
			continue // 半途失败的 add：check 会报告
		}
		var p render.Profile
		if err := json.Unmarshal(b, &p); err != nil {
			continue
		}
		out = append(out, Info{Name: name, Profile: p})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// List 返回已安装实例及其 master 是否在运行。
func (e Env) List() ([]Info, error) {
	list, err := e.installed()
	if err != nil {
		return nil, err
	}
	for i := range list {
		l := e.lay(list[i].Name)
		b, err := os.ReadFile(e.path(l.PidFile()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			continue
		}
		if _, err := os.Stat(e.path("/proc/" + strconv.Itoa(pid))); err == nil {
			list[i].Running, list[i].PID = true, pid
		}
	}
	return list, nil
}

// Check 返回本机实例布局中的问题（端口或名称冲突、缺少文件），没有问题时为空。
func (e Env) Check() []string {
	list, err := e.installed()
	if err != nil {
		return []string{err.Error()}
	}
	probs := conflicts(list)
	for _, in := range list {
		if in.Name == "" {
			continue
		}
		l := e.lay(in.Name)
		if in.Profile.Instance != in.Name {
			probs = append(probs, fmt.Sprintf("实例 %s 的 %s 写的是 %q", in.Name, layout.InstanceFile, in.Profile.Instance))
		}
		if err := in.Profile.Validate(); err != nil {
			probs = append(probs, fmt.Sprintf("实例 %s 的档案不合法：%v", in.Name, err))
		}
		need := []struct{ path, what string }{
			{l.CurrentConf(), "当前配置"},
			{l.AgentState(), "agent 状态目录"},
		}
		if !e.user() {
			need = append(need, struct{ path, what string }{l.SudoersFile(), "sudoers"})
		}
		for _, f := range need {
			if _, err := os.Stat(e.path(f.path)); err != nil {
				probs = append(probs, fmt.Sprintf("实例 %s 缺少 %s（%s）", in.Name, f.what, f.path))
			}
		}
	}
	// 半途失败的 add：有目录但没有档案
	entries, _ := os.ReadDir(e.path(e.lay("").InstancesDir()))
	for _, de := range entries {
		if _, err := os.Stat(e.path(e.lay("").InstancesDir() + "/" + de.Name() + "/" + layout.InstanceFile)); err != nil {
			probs = append(probs, fmt.Sprintf("目录 %s 没有 %s（未完整创建的实例，可执行 remove 清理）", de.Name(), layout.InstanceFile))
		}
	}
	sort.Strings(probs)
	return probs
}

// ports 返回一个实例占用的端口。
func ports(p render.Profile) map[string]string {
	return map[string]string{
		"tcp/" + strconv.Itoa(p.HTTP()):   "HTTP",
		"tcp/" + strconv.Itoa(p.HTTPS()):  "HTTPS",
		"tcp/" + strconv.Itoa(p.Status()): "状态页",
	}
}

func label(name string) string {
	if name == "" {
		return "默认实例"
	}
	return "实例 " + name
}

// conflicts 返回实例之间的名称与端口冲突。
func conflicts(list []Info) []string {
	var probs []string
	owner := map[string]string{}
	names := map[string]bool{}
	for _, in := range list {
		if names[in.Name] {
			probs = append(probs, label(in.Name)+" 已存在")
			continue
		}
		names[in.Name] = true
		for _, k := range sortedKeys(ports(in.Profile)) {
			if other, ok := owner[k]; ok {
				probs = append(probs, fmt.Sprintf("端口 %s：%s 与 %s 冲突", k, label(in.Name), other))
				continue
			}
			owner[k] = label(in.Name)
		}
	}
	return probs
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sudoers 返回该实例 agent 的 sudoers 内容：7 行，实例名写死，只有 test 与 upgrade-start 带参数通配。
func Sudoers(l layout.Layout) string { return SudoersAs(l, layout.WorkerUser()) }

// SudoersAs 与 Sudoers 相同，wrapper 以 runas 身份运行（兼容模式为 root，见 rootmaster.go）。
func SudoersAs(l layout.Layout, runas string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s —— 实例 %s 的 agent 只能操作本实例（%s instance add 生成，请勿手改）\n", l.SudoersFile(), l.Instance(), brand.ID())
	// agent 是没有终端的后台服务：部分系统（CentOS 7）的 sudo 默认 requiretty，只对该 agent 用户取消，不授予任何命令
	fmt.Fprintf(&b, "Defaults:%s !requiretty\n", l.AgentUser())
	for _, sub := range []string{"test *", "reload", "reopen", "quit", "upgrade-start *", "upgrade-commit", "upgrade-abort"} {
		fmt.Fprintf(&b, "%s ALL=(%s) NOPASSWD: %s --instance %s %s\n", l.AgentUser(), runas, l.WrapperBin(), l.Instance(), sub)
	}
	return b.String()
}

// withoutRequireTTY removes the CentOS 7-only sudo default override. Newer sudo
// versions reject the option instead of ignoring it, and do not need the
// override because requiretty is not enabled there.
func withoutRequireTTY(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Defaults:") && strings.HasSuffix(trimmed, " !requiretty") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ListeningFromProc 读取 root/proc/net/{tcp,tcp6,udp,udp6} 中处于监听状态的本地端口。
func ListeningFromProc(root string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range []struct{ name, proto, state string }{
		{"tcp", "tcp", "0A"}, {"tcp6", "tcp", "0A"}, // TCP_LISTEN
		{"udp", "udp", "07"}, {"udp6", "udp", "07"}, // 已绑定的 UDP 套接字
	} {
		fh, err := os.Open(filepath.Join(root, "proc", "net", f.name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != f.state {
				continue
			}
			i := strings.LastIndexByte(fields[1], ':')
			if i < 0 {
				continue
			}
			if port, err := strconv.ParseUint(fields[1][i+1:], 16, 16); err == nil {
				out[f.proto+"/"+strconv.FormatUint(port, 10)] = true
			}
		}
		_ = fh.Close()
	}
	return out, nil
}

// ReopenAll 让本机每个实例重新打开日志文件（供 logrotate 的 postrotate 调用）；返回第一个错误，但会处理完所有实例。
func (e Env) ReopenAll() error {
	list, err := e.installed()
	if err != nil {
		return err
	}
	var first error
	for _, in := range list {
		l := e.lay(in.Name)
		args := []string{"-s", "reopen"}
		if e.user() {
			args = []string{"-e", l.ErrorLog(), "-c", l.CurrentConf(), "-s", "reopen"}
		} else if in.Name != "" {
			args = []string{"-c", l.CurrentConf(), "-s", "reopen"}
		}
		bin := l.Bin()
		// 运行时不使用 root（D5）：`-s reopen` 会先以执行者身份打开错误日志，root 执行会留下 root 所有的日志文件，
		// 之后以数据面用户运行的 master 与 `-t` 无法写入；以 root 执行时（logrotate）经 runuser 以数据面用户发送。
		// 兼容模式下 master 是 root，信号只能由 root 发出
		if !e.user() && os.Geteuid() == 0 && !e.rootMaster() {
			args = append([]string{"-u", layout.WorkerUser(), "--", bin}, args...)
			bin = "runuser"
		}
		if err := e.Run(bin, args...); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", label(in.Name), err)
		}
	}
	return first
}
