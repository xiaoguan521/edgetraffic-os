// Package wrapper 实现提权入口 <品牌>-wrapper：agent 唯一的提权入口（规格书 5.5）。
//
// 安全要点：
//   - 只接受固定的子命令 test / reload / reopen / quit / upgrade-start / upgrade-commit / upgrade-abort；
//   - test 只接受一个 revision ID，且必须匹配 ^r[0-9]{6,12}$；upgrade-start 只接受 ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$；
//   - 升级包必须带厂商升级包签名（与 License 签名密钥分开），由本程序验签后才使用（upgrade.go；信任来自签名，不来自目录属主）；
//   - 二进制与配置路径全部由 internal/layout 按实例名计算，参数不参与路径以外的任何拼接；
//   - 用切片方式执行，清空环境变量，绝不经过 shell；
//   - 永远不提供 restart、-g、-p 或任意 -c。
package wrapper

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// DataplaneBin 返回默认 system 布局的数据面路径。生产命令使用实际 Layout
// 计算，因此自定义安装根目录不会经过这个兼容辅助函数。
func DataplaneBin() string { return layout.SystemAt("", "").Bin() }

// ReleasesDir 返回默认 system 布局的版本目录。升级与 wrapper 生产路径均从 Layout 计算。
func ReleasesDir() string { return layout.SystemAt("", "").ReleasesDir() }

// ExitUsage 是参数非法时的退出码（与 sysexits.h 的 EX_USAGE 一致）。
const ExitUsage = 64

// ErrUsage 表示调用参数不被允许。
var ErrUsage = errors.New("usage: " + brand.Wrapper() + " test <revision> | reload | reopen | quit | upgrade-start <version> | upgrade-commit | upgrade-abort")

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$`)

var revisionRe = regexp.MustCompile(`^r[0-9]{6,12}$`)

// Command 是一次经过校验、可直接执行的调用。
type Command struct {
	Root     string // user 模式的安装根目录（空串为 system 模式）
	Instance string // 可选的多实例名（匹配 ^[a-z0-9][a-z0-9_-]{0,31}$；默认单实例为空）
	Upgrade  string // 非空表示热升级动作：start | commit | abort（由 wrapper 自身执行，不调用外部命令）
	Version  string // upgrade-start 的目标版本
	Path     string
	Args     []string // 不含 argv[0]
	Env      []string
}

// SafeEnv 是执行 syedge 时使用的全部环境变量。
var SafeEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// Build 校验 argv（不含程序名）并返回 system 模式下要执行的命令。
// 任何不在白名单内的输入都返回 ErrUsage。
func Build(argv []string) (Command, error) { return BuildAt("", argv) }

// BuildAt 同 Build；root 非空时为 user 模式（安装根目录由 wrapper 自己所在的位置决定，见 RootFromExecutable，
// 从不来自参数）。user 模式下每条数据面命令都带 -e <实例错误日志>：编译期的 /var/log 下的路径普通用户不可写。
func BuildAt(root string, argv []string) (Command, error) {
	var instance string
	if len(argv) >= 2 && argv[0] == "--instance" {
		if argv[1] == "" || !layout.ValidInstance(argv[1]) { // 与 internal/layout 同一规则
			return Command{}, ErrUsage
		}
		instance = argv[1]
		argv = argv[2:]
	}
	if len(argv) == 0 {
		return Command{}, ErrUsage
	}
	l, err := layoutAt(root, instance)
	if err != nil {
		return Command{}, ErrUsage
	}
	bin, releasesDir, currentConf := l.Bin(), l.ReleasesDir(), ""
	early := []string{"-p", l.RuntimeRoot()}
	customSystem := l.Mode() == layout.ModeSystem && l.Root() != layout.LegacySystemRoot()
	if root != "" || customSystem {
		currentConf = l.CurrentConf() // user / 自定义 system 根目录：总是显式指定配置与错误日志
		early = append(early, "-e", l.ErrorLog())
	} else if instance != "" || l.Root() != layout.DefaultSystemRoot() {
		currentConf = l.CurrentConf()
	}
	c := Command{Root: root, Instance: instance, Path: bin, Env: SafeEnv}
	switch argv[0] {
	case "test":
		if len(argv) != 2 || !revisionRe.MatchString(argv[1]) {
			return Command{}, ErrUsage
		}
		conf := releasesDir + "/" + argv[1] + "/" + brand.ConfName()
		c.Args = append(append([]string{"-t", "-q"}, early...), "-c", conf)
		return c, nil
	case "reload", "reopen", "quit":
		if len(argv) != 1 {
			return Command{}, ErrUsage
		}
		c.Args = append([]string{}, early...)
		if currentConf != "" {
			c.Args = append(c.Args, "-c", currentConf)
		}
		c.Args = append(c.Args, "-s", argv[0])
		return c, nil
	case "upgrade-start":
		if len(argv) != 2 || !versionRe.MatchString(argv[1]) {
			return Command{}, ErrUsage
		}
		return Command{Root: root, Instance: instance, Upgrade: "start", Version: argv[1]}, nil
	case "upgrade-commit", "upgrade-abort":
		if len(argv) != 1 {
			return Command{}, ErrUsage
		}
		return Command{Root: root, Instance: instance, Upgrade: strings.TrimPrefix(argv[0], "upgrade-")}, nil
	default:
		return Command{}, ErrUsage
	}
}

// layoutAt 返回 system 模式（root 为空）或 user 模式的布局。
func layoutAt(root, instance string) (layout.Layout, error) {
	if root == "" {
		r, err := layout.LoadSystemRoot()
		if err != nil {
			return layout.Layout{}, err
		}
		return runtimeLayout(layout.SystemAt(r, instance), "")
	}
	l, err := layout.User(root, instance)
	if err != nil {
		return layout.Layout{}, err
	}
	return runtimeLayout(l, root)
}

// RootFromExecutable 由 wrapper 自身的位置判定模式（设计 0001 第 6.2 节）：
// 位于 system 模式的固定路径时返回空串；位于 <根目录>/bin/<品牌>-wrapper 时返回该根目录（user 模式）；
// 其他位置一律拒绝。安装根目录从不来自命令行参数。
func RootFromExecutable(exe string) (string, error) {
	systemWrapper := layout.SystemAt("", "").WrapperBin()
	if exe == systemWrapper {
		return "", nil
	}
	dir, name := filepath.Split(exe)
	dir = filepath.Clean(dir)
	if name != brand.Wrapper() || filepath.Base(dir) != "bin" {
		return "", fmt.Errorf("%s must be installed as %s or <root>/bin/%s", brand.Wrapper(), systemWrapper, brand.Wrapper())
	}
	root := filepath.Dir(dir)
	if _, err := layout.User(root, ""); err != nil {
		return "", fmt.Errorf("install root %q: %w", root, err)
	}
	return root, nil
}
