// Package layout 根据（品牌、模式、根目录、实例名）计算数据面实例的全部路径（设计 0001 第 3 节）。
//
// 除本包外，代码中不得拼写数据面、agent、wrapper 的路径。wrapper 只接收实例名，路径一律由本包计算，
// 这是提权入口不接受外部路径的前提。
package layout

import (
	"errors"
	"path"
	"regexp"
	"strconv"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// Mode 是安装模式。
type Mode string

const (
	// ModeSystem：root 安装，路径遵循 FHS（/opt、/run、/var/log、/var/lib）。
	ModeSystem Mode = "system"
	// ModeUser：普通用户安装，全部内容位于安装根目录之下。
	ModeUser Mode = "user"
)

var (
	instanceRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	rootRe     = regexp.MustCompile(`^(/[A-Za-z0-9._@+-]+)+$`)
)

// ValidInstance 判断实例名是否合法；空串表示默认实例，default 为保留名。
func ValidInstance(name string) bool {
	return name == "" || (name != "default" && instanceRe.MatchString(name))
}

// Layout 是一个实例的部署布局。零值不可用，请用 System 或 User 构造。
type Layout struct {
	mode     Mode
	root     string // 安装根目录：system 模式为 /opt/<品牌>
	instance string // 空串为默认实例
}

// System 返回 system 模式下的布局。实例名非法时 panic：调用方必须先用 ValidInstance 校验外部输入。
func System(instance string) Layout {
	if !ValidInstance(instance) {
		panic("layout: invalid instance name " + strconv.Quote(instance))
	}
	return Layout{mode: ModeSystem, root: "/opt/" + brand.ID(), instance: instance}
}

// User 返回 user 模式下的布局。root 必须是规范化的绝对路径。
func User(root, instance string) (Layout, error) {
	if !ValidInstance(instance) {
		return Layout{}, errors.New("invalid instance name")
	}
	if root == "/" || !rootRe.MatchString(root) || path.Clean(root) != root {
		return Layout{}, errors.New("install root must be a clean absolute path")
	}
	for _, seg := range splitPath(root) {
		if seg == "." || seg == ".." {
			return Layout{}, errors.New("install root must be a clean absolute path")
		}
	}
	return Layout{mode: ModeUser, root: root, instance: instance}, nil
}

func splitPath(p string) []string {
	var out []string
	for p != "/" && p != "." && p != "" {
		out = append(out, path.Base(p))
		p = path.Dir(p)
	}
	return out
}

// Mode 返回安装模式。
func (l Layout) Mode() Mode { return l.mode }

// Root 返回安装根目录（二进制、版本库、Lua 模块所在处，同机实例共用）。
func (l Layout) Root() string { return l.root }

// Instance 返回实例名，默认实例为空串。
func (l Layout) Instance() string { return l.instance }

// suffix 是具名实例在系统目录名后的后缀，例如 -blue。
func (l Layout) suffix() string {
	if l.instance == "" {
		return ""
	}
	return "-" + l.instance
}

// Prefix 返回实例前缀（数据面以 -p 指定）。
func (l Layout) Prefix() string {
	if l.instance == "" {
		return l.root
	}
	return path.Join(l.root, "instances", l.instance)
}

// ConfRoot 返回配置根目录（releases/ 与 current 软链所在处）。
func (l Layout) ConfRoot() string { return path.Join(l.Prefix(), "conf") }

// ReleasesDir 返回受管版本目录。
func (l Layout) ReleasesDir() string { return path.Join(l.ConfRoot(), "releases") }

// CurrentConf 返回当前生效的主配置文件。
func (l Layout) CurrentConf() string { return path.Join(l.ConfRoot(), "current", brand.ConfName()) }

// Bin 返回数据面二进制（同机共用，首次升级后为指向版本库的软链）。
func (l Layout) Bin() string { return path.Join(l.root, "sbin", brand.ID()) }

// Versions 返回数据面版本库（同机共用）。
func (l Layout) Versions() string { return path.Join(l.root, "versions") }

// Modules 返回动态模块目录（同机共用）。
func (l Layout) Modules() string { return path.Join(l.root, "modules") }

// RunDir 返回实例的运行目录（pid、锁）。
func (l Layout) RunDir() string {
	if l.mode == ModeUser {
		return path.Join(l.Prefix(), "run")
	}
	return "/run/" + brand.ID() + l.suffix()
}

// LogDir 返回实例的日志目录。
func (l Layout) LogDir() string {
	if l.mode == ModeUser {
		return path.Join(l.Prefix(), "logs")
	}
	return "/var/log/" + brand.ID() + l.suffix()
}

// CacheDir 返回实例的临时文件目录。
func (l Layout) CacheDir() string {
	if l.mode == ModeUser {
		return path.Join(l.Prefix(), "cache")
	}
	return "/var/cache/" + brand.ID() + l.suffix()
}

// PidFile 返回实例 master 的 pid 文件。
func (l Layout) PidFile() string { return path.Join(l.RunDir(), brand.PidName()) }

// ErrorLog 返回实例的错误日志。
func (l Layout) ErrorLog() string { return path.Join(l.LogDir(), "error.log") }

// AccessLog 返回实例的 JSON 访问日志。
func (l Layout) AccessLog() string { return path.Join(l.LogDir(), "access.json") }

// AgentState 返回该实例 agent 的身份与状态目录。
func (l Layout) AgentState() string {
	if l.mode == ModeUser {
		return path.Join(l.Prefix(), "agent")
	}
	return "/var/lib/" + brand.Agent() + l.suffix()
}

// AgentLock 返回该实例 agent 的发布互斥锁。
func (l Layout) AgentLock() string {
	if l.mode == ModeUser {
		return path.Join(l.RunDir(), "agent.lock")
	}
	return path.Join("/run/"+brand.Agent()+l.suffix(), "apply.lock")
}

// UpgradeIncoming 返回 agent 存放待安装升级包的目录。
func (l Layout) UpgradeIncoming() string { return path.Join(l.AgentState(), "upgrade") }

// WrapperBin 返回提权入口（user 模式下直接调用，不经过 sudo）。
func (l Layout) WrapperBin() string {
	if l.mode == ModeUser {
		return path.Join(l.root, "bin", brand.Wrapper())
	}
	return path.Join("/usr/libexec", brand.ID(), brand.Wrapper())
}

// AgentBin 返回 agent 二进制（同机共用）。
func (l Layout) AgentBin() string {
	if l.mode == ModeUser {
		return path.Join(l.root, "bin", brand.Agent())
	}
	return path.Join("/usr/bin", brand.Agent())
}

// StatusPath 返回本机状态页的路径（渲染器与 agent 必须一致）。
func StatusPath() string { return "/__" + brand.ID() + "_status" }

// StatusURL 返回本机状态页地址。
func (l Layout) StatusURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + StatusPath()
}

// InstanceFile 是实例前缀下的实例档案文件名（render.Profile 的 JSON），由 <品牌>-instance add 写入。
const InstanceFile = "instance.json"

// InstanceFile 返回本实例的档案路径。
func (l Layout) InstanceFile() string { return path.Join(l.Prefix(), InstanceFile) }

// Group 返回数据面与 agent 共用的系统组。
func Group() string { return brand.ID() }

// WorkerUser 返回数据面 worker 进程用户（同机实例共用）。
func WorkerUser() string { return brand.ID() }

// AgentUser 返回该实例 agent 的运行用户：每个具名实例单独一个，被攻破的 agent 不能操作其他实例（设计 0001 第 5.5 节）。
func (l Layout) AgentUser() string { return brand.Agent() + l.suffix() }

// DataplaneUnit 返回该实例数据面的 systemd 单元名。
func (l Layout) DataplaneUnit() string {
	if l.instance == "" {
		return brand.ID() + ".service"
	}
	return brand.ID() + "@" + l.instance + ".service"
}

// AgentUnit 返回该实例 agent 的 systemd 单元名。
func (l Layout) AgentUnit() string {
	if l.instance == "" {
		return brand.Agent() + ".service"
	}
	return brand.Agent() + "@" + l.instance + ".service"
}

// SudoersFile 返回该实例 agent 的 sudoers 文件。
func (l Layout) SudoersFile() string { return path.Join("/etc/sudoers.d", l.AgentUser()) }

// InstancesDir 返回具名实例前缀的父目录。
func (l Layout) InstancesDir() string { return path.Join(l.root, "instances") }
