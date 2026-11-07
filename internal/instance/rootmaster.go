package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// 兼容模式（root master，与传统 Web 服务器相同，见 internal/layout/rootmaster.go）：内核或 systemd 不支持 ambient 能力的系统
// （CentOS 7）上，非 root 的 master 无法绑定 80/443。这类系统上 master 由 systemd 以 root 启动，绑定端口后按编译进二进制的
// 默认用户启动 worker（与数据面用户相同，处理流量的进程仍不是 root）；wrapper 经 sudo 以 root 运行，才能向 master 发信号。

// rootMasterUnit 是数据面单元的附加配置：User / Group 写明 root（不用空值重置：老版本 systemd 对空值的处理不同）。边界集先清空再列出 root master 用到的能力：
// 只写 `CapabilityBoundingSet=` 会清空边界集，连 root 也无法绑定 80。
const rootMasterUnit = `# %s-instance nonroot 写入（兼容模式：master 以 root 运行，与传统 Web 服务器相同；worker 以 %s 运行）。请勿手改
[Service]
User=root
Group=root
AmbientCapabilities=
CapabilityBoundingSet=
CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_KILL CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_NET_BIND_SERVICE CAP_SYS_RESOURCE
NoNewPrivileges=no
`

// rootFirewallUnit：防火墙的加载与刷新同样需要 ambient 能力，兼容模式下以 root 运行（只保留需要的能力）。
const rootFirewallUnit = `# %s-instance nonroot 写入（兼容模式：防火墙以 root 运行）。请勿手改
[Service]
User=root
Group=root
AmbientCapabilities=
CapabilityBoundingSet=
CapabilityBoundingSet=CAP_NET_ADMIN CAP_DAC_OVERRIDE CAP_CHOWN CAP_FOWNER
NoNewPrivileges=no
`

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
	def := layout.System("")
	units := map[string]string{
		def.DataplaneUnit():                      rootMasterUnit,
		brand.ID() + "@.service":                 rootMasterUnit,
		brand.ID() + "-firewall.service":         rootFirewallUnit,
		brand.ID() + "-firewall-refresh.service": rootFirewallUnit,
	}
	for unit, tmpl := range units {
		dir := e.path("/etc/systemd/system/" + unit + ".d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		body := fmt.Sprintf(tmpl, brand.ID(), dp)
		if tmpl == rootFirewallUnit {
			body = fmt.Sprintf(tmpl, brand.ID())
		}
		if err := writeFile(filepath.Join(dir, "root-master.conf"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	// 运行时目录、日志与版本库仍交给数据面用户（worker 写日志、缓存；与默认模式一致，切回默认模式时无需再改）
	for _, in := range insts {
		l := layout.System(in.Name)
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
			units2 = append(units2, layout.System(in.Name).AgentUnit())
		}
	}
	_ = e.Run("systemctl", append([]string{"try-restart"}, units2...)...)
	return nil
}

// rootSudoersDefault 把默认实例的 sudoers（软件包提供，以数据面用户运行 wrapper）改为以 root 运行，7 行内容不变。
func (e Env) rootSudoersDefault() error {
	l := layout.System("")
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
