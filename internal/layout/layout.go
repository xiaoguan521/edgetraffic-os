// Package layout 根据（品牌、模式、根目录、实例名）计算数据面实例的全部路径（设计 0001 第 3 节）。
//
// 除本包外，代码中不得拼写数据面、agent、wrapper 的路径。wrapper 只接收实例名，路径一律由本包计算，
// 这是提权入口不接受外部路径的前提。
package layout

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

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
	systemRoot struct {
		sync.Once
		root string
		err  error
	}
)

// ValidInstance 判断实例名是否合法；空串表示默认实例，default 为保留名。
func ValidInstance(name string) bool {
	return name == "" || (name != "default" && instanceRe.MatchString(name))
}

// Layout 是一个实例的部署布局。零值不可用，请用 System、SystemAt 或 User 构造。
type Layout struct {
	mode        Mode
	root        string // 安装根目录：system 模式为 /opt/<品牌>
	instance    string // 空串为默认实例
	independent bool   // 具名实例拥有独立的数据面二进制、版本库和模块
}

// defaultSystemRoot 由发布构建通过 -ldflags 注入；空值时保持历史默认路径。
// 这样 node、agent、instance 和 wrapper 与数据面的编译期前缀保持一致。
var defaultSystemRoot string

// DefaultSystemRoot 返回未设置 /etc/syedge/prefix 时使用的安装根目录。
func DefaultSystemRoot() string {
	if defaultSystemRoot != "" {
		return defaultSystemRoot
	}
	return LegacySystemRoot()
}

// LegacySystemRoot 是历史版本和未显式指定根目录的节点组档案所使用的固定路径。
func LegacySystemRoot() string { return "/opt/" + brand.ID() }

// System 返回 system 模式下的布局。根目录从 /etc/syedge/prefix 读取一次；标记
// 缺失时使用 DefaultSystemRoot。实例名非法或标记无效时 panic：命令入口应在
// 调用前用 LoadSystemRoot 显式报告错误。
func System(instance string) Layout {
	root, err := LoadSystemRoot()
	if err != nil {
		panic("layout: invalid system root: " + err.Error())
	}
	return SystemAt(root, instance)
}

// SystemAt 返回指定安装根目录下的 system 模式布局。它是纯函数，不读取本机
// 标记文件，供控制台渲染和测试使用。root 为空表示固定的默认根目录。
func SystemAt(root, instance string) Layout {
	if !ValidInstance(instance) {
		panic("layout: invalid instance name " + strconv.Quote(instance))
	}
	if root == "" {
		root = DefaultSystemRoot()
	}
	if err := ValidateSystemRoot(root); err != nil {
		panic("layout: invalid install root: " + err.Error())
	}
	return Layout{mode: ModeSystem, root: root, instance: instance}
}

// ValidateInstallRoot 校验安装根目录的词法形式。它不检查目录是否存在或属主，
// 因而可用于控制台保存尚未部署到本机的节点组档案。
func ValidateInstallRoot(root string) error {
	if root == "/" || !rootRe.MatchString(root) || path.Clean(root) != root {
		return errors.New("install root must be a clean absolute path")
	}
	for _, seg := range splitPath(root) {
		if seg == "." || seg == ".." {
			return errors.New("install root must be a clean absolute path")
		}
	}
	return nil
}

// ValidateSystemRoot 要求 system 模式使用专用目录，避免安装和卸载时触及系统
// 文件；/home/<子目录> 可用于 system 模式，服务单元会适配该路径。
func ValidateSystemRoot(root string) error {
	if err := ValidateInstallRoot(root); err != nil {
		return err
	}
	for _, dir := range []string{"/usr", "/usr/local", "/opt", "/var", "/var/lib", "/var/cache", "/home"} {
		if root == dir {
			return fmt.Errorf("system install root %s must be a dedicated directory", root)
		}
	}
	for _, dir := range []string{
		"/bin", "/sbin", "/lib", "/lib64", "/boot", "/dev", "/etc", "/proc", "/run", "/sys",
		"/root", "/var/log", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/usr/libexec", "/usr/share",
		"/usr/local/bin", "/usr/local/sbin", "/usr/local/lib", "/usr/local/lib64", "/usr/local/libexec", "/usr/local/share",
	} {
		if root == dir || strings.HasPrefix(root, dir+"/") {
			return fmt.Errorf("system install root %s is inside a protected directory %s", root, dir)
		}
	}
	if root != DefaultSystemRoot() && strings.HasPrefix(root, DefaultSystemRoot()+"/") {
		return fmt.Errorf("system install root %s is inside the default installation", root)
	}
	if root != LegacySystemRoot() && strings.HasPrefix(root, LegacySystemRoot()+"/") {
		return fmt.Errorf("system install root %s is inside the legacy installation", root)
	}
	return nil
}

// User 返回 user 模式下的布局。root 必须是规范化的绝对路径。
func User(root, instance string) (Layout, error) {
	if !ValidInstance(instance) {
		return Layout{}, errors.New("invalid instance name")
	}
	if err := ValidateInstallRoot(root); err != nil {
		return Layout{}, err
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

// WithIndependentRuntime 仅改变数据面程序、版本库和模块的归属，实例前缀与配置目录不变。
func (l Layout) WithIndependentRuntime() Layout {
	l.independent = true
	return l
}

// IndependentRuntime 报告实例是否有独立的数据面运行文件。
func (l Layout) IndependentRuntime() bool { return l.independent }

func (l Layout) runtimeRoot() string {
	if l.independent {
		return l.Prefix()
	}
	return l.root
}

// RuntimeRoot 是二进制及其动态模块所在的可信前缀。
func (l Layout) RuntimeRoot() string { return l.runtimeRoot() }

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

// Bin 返回数据面二进制（首次升级后为指向对应版本库的软链）。
func (l Layout) Bin() string { return path.Join(l.runtimeRoot(), "sbin", brand.ID()) }

// Versions 返回实例使用的共享或独立数据面版本库。
func (l Layout) Versions() string { return path.Join(l.runtimeRoot(), "versions") }

// Modules 返回实例使用的共享或独立动态模块目录。
func (l Layout) Modules() string { return path.Join(l.runtimeRoot(), "modules") }

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
	if l.root != LegacySystemRoot() {
		return path.Join(l.Prefix(), "logs")
	}
	return "/var/log/" + brand.ID() + l.suffix()
}

// CacheDir 返回实例的临时文件目录。
func (l Layout) CacheDir() string {
	if l.mode == ModeUser {
		return path.Join(l.Prefix(), "cache")
	}
	if l.root != LegacySystemRoot() {
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
	if l.root != LegacySystemRoot() {
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

// VersionPath 返回本机状态服务上报运行版本的路径：由 worker 返回编译时的版本号，
// 即本实例实际运行的二进制版本（同机多实例共用软链时可能与软链指向的版本不同，设计 0001 第 5.6 节）。
func VersionPath() string { return "/__" + brand.ID() + "_version" }

// VersionURL 返回本机运行版本的地址。
func (l Layout) VersionURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + VersionPath()
}

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

// PrefixMarker 返回 system 模式安装根目录标记文件的绝对路径。
func PrefixMarker() string { return "/etc/" + brand.ID() + "/prefix" }

// LoadSystemRoot 读取本机 system 模式安装根目录。缺少标记表示历史默认安装；
// 一旦标记存在但不满足安全校验则返回错误。结果在进程内缓存，避免运行过程中
// 通过替换标记改变路径。
func LoadSystemRoot() (string, error) {
	systemRoot.Do(func() {
		systemRoot.root, systemRoot.err = LoadSystemRootAt("/")
	})
	if systemRoot.err != nil {
		return "", systemRoot.err
	}
	return systemRoot.root, nil
}

// LoadSystemRootAt 是 LoadSystemRoot 的可注入版本：fsroot 是模拟的文件系统根，
// 生产调用使用 "/"。它不使用全局缓存。
func LoadSystemRootAt(fsroot string) (string, error) {
	if fsroot == "" {
		fsroot = "/"
	}
	marker := filepath.Join(fsroot, filepath.FromSlash(PrefixMarker()))
	fi, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		root := DefaultSystemRoot()
		if root != LegacySystemRoot() {
			legacy := filepath.Join(fsroot, filepath.FromSlash(LegacySystemRoot()), "conf", "current")
			if _, e := os.Lstat(legacy); e == nil {
				current := filepath.Join(fsroot, filepath.FromSlash(root), "conf", "current")
				if _, e = os.Lstat(current); e == nil {
					return "", fmt.Errorf("both legacy and default installations exist; specify %s", PrefixMarker())
				} else if !errors.Is(e, os.ErrNotExist) {
					return "", e
				}
				root = LegacySystemRoot()
			} else if !errors.Is(e, os.ErrNotExist) {
				return "", e
			}
		}
		if err := ValidateSystemRoot(root); err != nil {
			return "", err
		}
		// 生产文件系统上的隐式默认路径与标记路径一样需要验证父目录。
		// 注入的测试文件系统由调用者单独使用 CheckSystemRootAt 验证所有权。
		if fsroot == "/" {
			if err := CheckSystemRootAt(fsroot, root); err != nil {
				return "", err
			}
		}
		return root, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", PrefixMarker(), err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 || !ownedByRoot(fi) {
		return "", fmt.Errorf("%s must be a root-owned regular file with mode 0644", PrefixMarker())
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", PrefixMarker(), err)
	}
	s := string(b)
	if strings.HasSuffix(s, "\r\n") {
		s = strings.TrimSuffix(s, "\r\n")
	} else if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(s, "\n")
	}
	if strings.ContainsAny(s, "\r\n") || strings.TrimSpace(s) != s {
		return "", fmt.Errorf("%s must contain one clean absolute path", PrefixMarker())
	}
	if err := CheckSystemRootAt(fsroot, s); err != nil {
		return "", err
	}
	return s, nil
}

// CheckSystemRoot 校验一个 system 模式安装根目录在本机上的安全属性。
// 根目录本身可以尚不存在（安装脚本会先写标记再创建目录）；已存在的根目录
// 及父目录必须由 root 所有且不可被组或其他用户写入；粘滞 /home 可有
// 写权限，但下一级目录必须已经存在且由 root 所有。
func CheckSystemRoot(root string) error { return CheckSystemRootAt("/", root) }

// CheckSystemRootAt 是 CheckSystemRoot 的可注入版本。
func CheckSystemRootAt(fsroot, root string) error {
	if fsroot == "" {
		fsroot = "/"
	}
	if err := ValidateSystemRoot(root); err != nil {
		return err
	}
	clean := filepath.Join(fsroot, filepath.FromSlash(root))
	childTrusted := false
	for p := clean; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if p == filepath.Join(fsroot, string(filepath.Separator)) || p == fsroot {
				break
			}
			childTrusted = false
			continue
		}
		if err != nil {
			return fmt.Errorf("check install root %s: %w", root, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install root %s or its parents must be non-symlink directories", root)
		}
		rootOwned := ownedByRoot(fi)
		if !rootOwned || fi.Mode().Perm()&0o022 != 0 {
			stickyHome := rootOwned && p == filepath.Join(fsroot, "home") && fi.Mode()&os.ModeSticky != 0
			if stickyHome && !childTrusted {
				return fmt.Errorf("install root %s: sticky /home requires its first child directory to exist and be root-owned", root)
			}
			// 粘滞位阻止非 root 删除或替换 root 所有的下一级目录；不放宽其他父目录。
			if !stickyHome || !childTrusted {
				return fmt.Errorf("install root %s: %s must be root-owned and not writable by group or others (mode %04o, root-owned %t)", root, p, fi.Mode().Perm(), rootOwned)
			}
		}
		childTrusted = true
		if p == filepath.Join(fsroot, string(filepath.Separator)) || p == fsroot {
			break
		}
	}
	return nil
}
