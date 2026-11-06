package render

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// Profile 是节点组的实例档案（设计 0001 第 3.4 节）：决定节点上的路径与监听端口。
// 同组节点使用同一档案，因此同组节点拿到的配置包逐字节相同。零值即默认实例，渲染结果与引入档案前完全一致。
type Profile struct {
	Instance   string `json:"instance,omitempty"`    // 空串为默认实例
	Mode       string `json:"mode,omitempty"`        // system（默认）| user
	Root       string `json:"root,omitempty"`        // user 模式的安装根目录
	HTTPPort   int    `json:"http_port,omitempty"`   // 默认 80
	HTTPSPort  int    `json:"https_port,omitempty"`  // 默认 443
	StatusPort int    `json:"status_port,omitempty"` // 本机状态页，默认 8081
	NoFile     int    `json:"nofile,omitempty"`      // worker_rlimit_nofile，默认 65535
}

// ErrProfile 表示实例档案不合法。
var ErrProfile = errors.New("invalid instance profile")

const (
	maxNoFile     = 1 << 20
	minUserPort   = 1024 // 普通用户只能监听 1024 及以上端口
	sshPort       = 22
	defaultNoFile = 65535
)

func (p Profile) mode() layout.Mode {
	if p.Mode == string(layout.ModeUser) {
		return layout.ModeUser
	}
	return layout.ModeSystem
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// HTTP 返回明文监听端口。
func (p Profile) HTTP() int { return orDefault(p.HTTPPort, 80) }

// HTTPS 返回 TLS 监听端口。
func (p Profile) HTTPS() int { return orDefault(p.HTTPSPort, 443) }

// Status 返回本机状态页端口。
func (p Profile) Status() int { return orDefault(p.StatusPort, 8081) }

// Files 返回 worker_rlimit_nofile。
func (p Profile) Files() int { return orDefault(p.NoFile, defaultNoFile) }

// Validate 校验档案。
func (p Profile) Validate() error {
	if p.Mode != "" && p.Mode != string(layout.ModeSystem) && p.Mode != string(layout.ModeUser) {
		return fmt.Errorf("%w: mode must be system or user", ErrProfile)
	}
	if !layout.ValidInstance(p.Instance) {
		return fmt.Errorf("%w: instance name %q", ErrProfile, p.Instance)
	}
	if p.mode() == layout.ModeSystem && p.Root != "" {
		return fmt.Errorf("%w: root is only used in user mode", ErrProfile)
	}
	if _, err := p.Layout(); err != nil {
		return fmt.Errorf("%w: %v", ErrProfile, err)
	}
	if p.NoFile < 0 || p.NoFile > maxNoFile {
		return fmt.Errorf("%w: nofile out of range", ErrProfile)
	}
	seen := map[int]string{}
	for _, c := range []struct {
		name string
		port int
	}{{"http_port", p.HTTP()}, {"https_port", p.HTTPS()}, {"status_port", p.Status()}} {
		switch {
		case c.port < 1 || c.port > 65535:
			return fmt.Errorf("%w: %s %d out of range", ErrProfile, c.name, c.port)
		case c.port == sshPort:
			return fmt.Errorf("%w: %s must not be %d", ErrProfile, c.name, sshPort)
		case p.mode() == layout.ModeUser && c.port < minUserPort:
			return fmt.Errorf("%w: user mode cannot listen on %s %d (< %d)", ErrProfile, c.name, c.port, minUserPort)
		}
		if other, dup := seen[c.port]; dup {
			return fmt.Errorf("%w: %s and %s are both %d", ErrProfile, other, c.name, c.port)
		}
		seen[c.port] = c.name
	}
	return nil
}

// Layout 返回档案对应的部署布局。
func (p Profile) Layout() (layout.Layout, error) {
	if p.mode() == layout.ModeUser {
		return layout.User(p.Root, p.Instance)
	}
	if !layout.ValidInstance(p.Instance) {
		return layout.Layout{}, errors.New("invalid instance name")
	}
	return layout.System(p.Instance), nil
}

// lay 返回已校验档案的布局；只在 Validate 通过后调用。
func (p Profile) lay() layout.Layout {
	l, err := p.Layout()
	if err != nil {
		panic("render: profile used before validation: " + err.Error())
	}
	return l
}

// ReleasesDir 返回节点上版本目录的父目录（带结尾斜杠）。
func (p Profile) ReleasesDir() string { return p.lay().ReleasesDir() + "/" }

// reservedPorts 返回四层代理不能占用的端口。
func (p Profile) reservedPorts() map[int]bool {
	return map[int]bool{sshPort: true, p.HTTP(): true, p.HTTPS(): true, p.Status(): true}
}

// profileView 是模板中 .P 的内容。
type profileView struct {
	NoFile      int
	PidFile     string
	ErrorLog    string
	AccessLog   string
	StreamLog   string // 四层代理访问日志
	CustomTemp  bool   // 临时目录与编译期默认值不同，需要显式写出 *_temp_path
	TempDir     string // 临时目录
	HTTP        int
	HTTPS       int
	Status      int
	StatusPath  string
	VersionPath string // 运行版本（worker 返回 $<品牌>_version）
	VersionVar  string
	SitesDir    string // 当前版本静态站点目录（带结尾斜杠）
	ReleasesDir string // 版本目录的父目录（带结尾斜杠）
	ModulesDir  string // load_module 的路径前缀：system 模式相对编译期前缀（modules/），user 模式为绝对路径
	LuaDir      string // Lua 附加包目录（绝对路径）
}

func (p Profile) view() profileView {
	l := p.lay()
	mods := "modules/"
	if l.Mode() == layout.ModeUser {
		mods = l.Modules() + "/"
	}
	return profileView{
		NoFile:  p.Files(),
		PidFile: l.PidFile(), ErrorLog: l.ErrorLog(), AccessLog: l.AccessLog(), StreamLog: l.LogDir() + "/stream.log",
		CustomTemp: l.CacheDir() != layout.System("").CacheDir(), TempDir: l.CacheDir(),
		HTTP: p.HTTP(), HTTPS: p.HTTPS(), Status: p.Status(), StatusPath: layout.StatusPath(),
		VersionPath: layout.VersionPath(), VersionVar: "$" + brand.ID() + "_version",
		SitesDir: l.ConfRoot() + "/current/sites/", ReleasesDir: p.ReleasesDir(),
		ModulesDir: mods, LuaDir: l.Modules() + "/lua",
	}
}

// P 供模板使用：{{ .P.PidFile }}。
func (in Input) P() profileView { return in.Profile.view() }

// RebaseRevisionFor 把配置文件中指向本实例 oldRev 版本目录的绝对路径改写为 newRev。
func RebaseRevisionFor(p Profile, files map[string][]byte, oldRev, newRev string) map[string][]byte {
	return rebase(files, p.ReleasesDir(), oldRev, newRev)
}

func rebase(files map[string][]byte, releases, oldRev, newRev string) map[string][]byte {
	out := make(map[string][]byte, len(files))
	from, to := releases+oldRev+"/", releases+newRev+"/"
	for n, b := range files {
		if strings.HasSuffix(n, ".conf") && revisionRe.MatchString(oldRev) && revisionRe.MatchString(newRev) {
			b = []byte(strings.ReplaceAll(string(b), from, to))
		}
		out[n] = b
	}
	return out
}

// Effective 返回补齐默认值后的档案（端口与文件数），用于比较两份档案是否等价。
func (p Profile) Effective() Profile {
	if p.Mode == "" {
		p.Mode = "system"
	}
	p.HTTPPort, p.HTTPSPort, p.StatusPort, p.NoFile = p.HTTP(), p.HTTPS(), p.Status(), p.Files()
	return p
}

// SameInstance 报告两份档案是否描述同一个实例（实例名、模式、根目录、端口一致；nofile 不参与比较，节点可按本机限制调整）。
func (p Profile) SameInstance(o Profile) bool {
	a, b := p.Effective(), o.Effective()
	a.NoFile, b.NoFile = 0, 0
	return a == b
}
