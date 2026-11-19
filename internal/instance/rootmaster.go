package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// 兼容模式（root master，与传统 Web 服务器相同，见 internal/layout/rootmaster.go）：内核或 systemd 不支持 ambient 能力的系统
// （CentOS 7）上，非 root 的 master 无法绑定 80/443。这类系统上 master 由 systemd 以 root 启动，绑定端口后按编译进二进制的
// 默认用户启动 worker（与数据面用户相同，处理流量的进程仍不是 root）；wrapper 经 sudo 以 root 运行，才能向 master 发信号。

// 兼容模式的单元不用附加配置（.d/*.conf），而是由软件包单元生成完整的覆盖单元 /etc/systemd/system/<单元>：
// systemd 229 以下（如 CentOS 7.0/7.1 的 208）对多行 CapabilityBoundingSet= 取交集、不支持空值重置，
// 附加配置无法放宽软件包单元中的 CapabilityBoundingSet=CAP_NET_BIND_SERVICE，master 会拿不到 CAP_SETGID / CAP_SETUID，
// worker 切换到数据面用户时失败（setgid … Operation not permitted）。覆盖单元只替换运行身份与能力相关的行，
// 其余内容与软件包单元一致；每次 nonroot（软件包安装与升级、install.sh）重新生成，因此跟随软件包更新。

// rootMasterCaps 是 root master 用到的能力（绑定端口、切换 worker 用户、创建目录与文件、向 worker 发信号、调整限制）。
const rootMasterCaps = "CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_KILL CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_NET_BIND_SERVICE CAP_SYS_RESOURCE"

// rootFirewallCaps：防火墙的加载与刷新同样需要 ambient 能力，兼容模式下以 root 运行（只保留需要的能力）。
const rootFirewallCaps = "CAP_NET_ADMIN CAP_DAC_OVERRIDE CAP_CHOWN CAP_FOWNER"

// legacyRootMasterDropIn 是 v0.4.1 及以前兼容模式写入的附加配置，现由覆盖单元取代。
const legacyRootMasterDropIn = "root-master.conf"

// vendorUnitDirs 是软件包安装单元文件的目录（rpm / deb）；tar 包（install.sh）直接安装在 /etc/systemd/system。
var vendorUnitDirs = []string{"/usr/lib/systemd/system", "/lib/systemd/system"}

func overrideHeader() string { return "# " + brand.ID() + "-instance nonroot 生成" }

// rootMasterUnits 返回兼容模式需要覆盖的单元及其能力。
func rootMasterUnits() map[string]string {
	return map[string]string{
		brand.ID() + ".service":                  rootMasterCaps,
		brand.ID() + "@.service":                 rootMasterCaps,
		brand.ID() + "-firewall.service":         rootFirewallCaps,
		brand.ID() + "-firewall-refresh.service": rootFirewallCaps,
	}
}

// compatUnit 由软件包单元生成兼容模式的覆盖单元：去掉运行身份与能力相关的行，在 [Service] 开头写入 root 运行与能力边界。
// 对已生成的覆盖单元再执行一次得到相同结果（tar 包安装时在原文件上生成）。
func compatUnit(src, caps, dp string) string {
	drop := []string{"User=", "Group=", "AmbientCapabilities=", "CapabilityBoundingSet=", "NoNewPrivileges="}
	var b strings.Builder
	b.WriteString(overrideHeader() + "（兼容模式：master 以 root 运行，与传统 Web 服务器相同；worker 以 " + dp + " 运行）。请勿手改：\n")
	b.WriteString("# 每次执行 " + brand.ID() + "-instance nonroot 都会按软件包单元重新生成；删除 " + layout.RootMasterMarker() + " 后执行 nonroot 恢复\n")
	lines := strings.Split(strings.TrimRight(src, "\n"), "\n")
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, overrideHeader()) || strings.HasPrefix(t, "# 每次执行 "+brand.ID()+"-instance nonroot") {
			continue
		}
		skip := false
		for _, d := range drop {
			if strings.HasPrefix(t, d) {
				skip = true
			}
		}
		if skip {
			continue
		}
		b.WriteString(ln + "\n")
		if t == "[Service]" {
			b.WriteString("User=root\nGroup=root\nCapabilityBoundingSet=" + caps + "\nNoNewPrivileges=no\n")
		}
	}
	return b.String()
}

// unitOriginDir 保存 tar 包安装的原始单元副本：兼容模式与自定义根目录会在 /etc/systemd/system 里原位改写单元，
// 改写前不留副本就无法再得到软件包版本（解压目录可能早已删除），切回普通模式时会继续沿用 User=root。
func unitOriginDir() string { return "/etc/" + brand.ID() + "/unit-origin" }

// generatedUnit 报告内容是否由本程序生成（兼容模式或自定义根目录），而不是软件包原样。
func generatedUnit(b []byte) bool {
	s := string(b)
	return strings.HasPrefix(s, overrideHeader()) || strings.HasPrefix(s, prefixUnitHeader())
}

// unitSource 找到单元的软件包版本：先找软件包目录（rpm / deb）；tar 包安装时 /etc/systemd/system 里未被改写的文件就是软件包版本，
// 同时记下原始副本（重新执行 install.sh 升级后会刷新）；文件已被改写时用记录的副本。返回路径与内容。
// 只有既没有软件包目录、也没有原始副本时，才退回已改写的文件本身（升级到本版本之前进入兼容模式的节点）。
func (e Env) unitSource(unit string) (string, []byte, bool) {
	for _, d := range vendorUnitDirs {
		p := d + "/" + unit
		if b, err := os.ReadFile(e.path(p)); err == nil {
			return p, b, true
		}
	}
	etc := "/etc/systemd/system/" + unit
	cur, curErr := os.ReadFile(e.path(etc))
	origin := unitOriginDir() + "/" + unit
	if curErr == nil && !generatedUnit(cur) {
		if err := os.MkdirAll(e.path(unitOriginDir()), 0o755); err == nil {
			_ = writeFile(e.path(origin), cur, 0o644)
		}
		return etc, cur, true
	}
	if b, err := os.ReadFile(e.path(origin)); err == nil {
		return origin, b, true
	}
	if curErr == nil {
		return etc, cur, true
	}
	return "", nil, false
}

// writeCompatUnits 生成覆盖单元并删除旧版附加配置。
func (e Env) writeCompatUnits() error {
	dp := layout.WorkerUser()
	units := rootMasterUnits()
	names := make([]string, 0, len(units))
	for u := range units {
		names = append(names, u)
	}
	sort.Strings(names)
	for _, unit := range names {
		_ = os.Remove(e.path("/etc/systemd/system/" + unit + ".d/" + legacyRootMasterDropIn))
		src, b, ok := e.unitSource(unit)
		if !ok {
			continue // 未安装该单元（例如只装了数据面）
		}
		source := string(b)
		if root, custom := e.customSystemRoot(); custom {
			source = prefixUnit(source, root, e.homeSandbox())
		}
		dst := e.path("/etc/systemd/system/" + unit)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := writeFile(dst, []byte(compatUnit(source, units[unit], dp)), 0o644); err != nil {
			return err
		}
		e.say("   %s：已按 %s 生成兼容模式单元 /etc/systemd/system/%s", unit, src, unit)
	}
	return nil
}

// removeCompatUnits 离开兼容模式：删除生成的覆盖单元（软件包单元随即生效）与旧版附加配置。
// tar 包安装时覆盖单元就是安装的单元本身，不能删除：提示重新执行 install.sh 恢复。
func (e Env) removeCompatUnits() {
	for unit := range rootMasterUnits() {
		_ = os.Remove(e.path("/etc/systemd/system/" + unit + ".d/" + legacyRootMasterDropIn))
		p := e.path("/etc/systemd/system/" + unit)
		b, err := os.ReadFile(p)
		if err != nil || !strings.HasPrefix(string(b), overrideHeader()) {
			continue
		}
		vendor := false
		for _, d := range vendorUnitDirs {
			if _, err := os.Stat(e.path(d + "/" + unit)); err == nil {
				vendor = true
			}
		}
		if vendor {
			_ = os.Remove(p)
			e.say("已删除兼容模式单元 /etc/systemd/system/%s，恢复软件包单元", unit)
		} else if orig, err := os.ReadFile(e.path(unitOriginDir() + "/" + unit)); err == nil {
			if err := writeFile(p, orig, 0o644); err != nil {
				e.say("警告：无法恢复 /etc/systemd/system/%s：%v；请重新执行发布包中的 install.sh 恢复", unit, err)
				continue
			}
			e.say("已用安装时记录的原始单元恢复 /etc/systemd/system/%s", unit)
		} else {
			e.say("警告：/etc/systemd/system/%s 是兼容模式单元（tar 包安装），没有安装时记录的原始副本：请重新执行发布包中的 install.sh 恢复", unit)
		}
	}
}

// rootMaster 报告本机是否处于兼容模式（标记文件存在且属于 root）。
func (e Env) rootMaster() bool { return !e.user() && layout.RootMasterAt(e.Root) }

// wrapperRunas 是 sudoers 中 wrapper 的运行身份。
func (e Env) wrapperRunas() string {
	if e.rootMaster() || e.RootMaster || (e.Ambient != nil && !e.Ambient()) {
		return "root"
	}
	return layout.WorkerUser()
}

// asWrapperUser 返回以 wrapper 的运行身份执行命令的参数（第一个元素为命令名）。
func (e Env) asWrapperUser(cmd ...string) []string {
	if e.wrapperRunas() == "root" {
		return cmd
	}
	return append([]string{"runuser", "-u", layout.WorkerUser(), "--"}, cmd...)
}

// rootMasterSetup：进入（或保持）兼容模式。可重复执行；不停止、不重启数据面：已运行的 master 不变，
// 新的单元配置在下次启动时生效（以 root 运行的 master 本来就满足兼容模式）。
func (e Env) rootMasterSetup(insts []Info) error {
	dp := layout.WorkerUser()
	e.say("== 兼容模式：本机内核或 systemd 不支持 ambient 能力（或已选择 --root-master）")
	e.say("   master 以 root 运行并绑定端口，worker 以 %s 运行（与传统 Web 服务器相同）；wrapper 经 sudo 以 root 运行", dp)
	marker := e.path(layout.RootMasterMarker())
	if err := writeFile(marker, []byte("# 兼容模式：master 以 root 运行（"+brand.ID()+"-instance nonroot 写入）。删除本文件并重新执行 nonroot 可恢复运行时不使用 root\n"), 0o644); err != nil {
		return err
	}
	_ = os.Chown(marker, 0, 0)
	def := e.lay("")
	// 兼容模式同样需要 agent 与防火墙 path 单元使用自定义根目录；
	// 随后再为数据面与防火墙服务叠加 root-master 能力配置。
	if _, custom := e.customSystemRoot(); custom {
		if err := e.writePrefixUnits(); err != nil {
			return err
		}
	}
	if err := e.writeCompatUnits(); err != nil {
		return err
	}
	// 运行时目录、日志与版本库仍交给数据面用户（worker 写日志、缓存；与默认模式一致，切回默认模式时无需再改）
	for _, in := range insts {
		l := e.lay(in.Name)
		for _, d := range []string{l.RunDir(), l.LogDir(), l.CacheDir()} {
			if err := os.MkdirAll(e.path(d), 0o755); err != nil {
				return err
			}
			if err := e.Run("chown", "-R", dp+":"+dp, d); err != nil {
				return err
			}
		}
		if in.Name != "" {
			if err := e.writeSudoers(l); err != nil {
				return err
			}
		}
	}
	if err := e.rootSudoersDefault(); err != nil {
		return err
	}
	if err := e.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	units2 := []string{def.AgentUnit()}
	for _, in := range insts {
		if in.Name != "" {
			units2 = append(units2, e.lay(in.Name).AgentUnit())
		}
	}
	_ = e.Run("systemctl", append([]string{"try-restart"}, units2...)...)
	return nil
}

// rootSudoersDefault 把默认实例的 sudoers（软件包提供，以数据面用户运行 wrapper）改为以 root 运行，7 行内容不变。
func (e Env) rootSudoersDefault() error {
	l := e.lay("")
	sp := e.path(l.SudoersFile())
	b, err := os.ReadFile(sp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 只装了数据面
		}
		return err
	}
	s := strings.ReplaceAll(string(b), "ALL=("+layout.WorkerUser()+")", "ALL=(root)")
	if s == string(b) {
		return nil
	}
	tmp := sp + ".new"
	if err := writeFile(tmp, []byte(s), 0o440); err != nil {
		return err
	}
	if err := e.Run("visudo", "-cf", l.SudoersFile()+".new"); err != nil {
		// Ubuntu and newer sudo releases reject the CentOS 7-only requiretty
		// option. Retry without that optional override; any other syntax error
		// still fails the second validation.
		fallback := withoutRequireTTY(s)
		if fallback == s || writeFile(tmp, []byte(fallback), 0o440) != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("sudoers 校验失败: %w", err)
		}
		if retryErr := e.Run("visudo", "-cf", l.SudoersFile()+".new"); retryErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("sudoers 校验失败: %w", retryErr)
		}
	}
	return os.Rename(tmp, sp)
}

// SystemdAtLeast 解析 `systemctl --version` 的第一行（"systemd 219"、"systemd 252 (252.16-1.el9)"），报告版本是否不低于 v。
func SystemdAtLeast(out string, v int) bool {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "systemd" {
		return false
	}
	n := 0
	for _, c := range f[1] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n >= v
}
