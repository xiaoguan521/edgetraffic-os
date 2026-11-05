// Package wrapper 实现提权入口 <品牌>-wrapper：agent 唯一的提权入口（规格书 5.5）。
//
// 安全要点：
//   - 只接受固定的子命令 test / reload / reopen / quit / upgrade-start / upgrade-commit / upgrade-abort；
//   - test 只接受一个 revision ID，且必须匹配 ^r[0-9]{6,12}$；upgrade-start 只接受 ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$；
//   - 升级包必须带厂商升级包签名（与 License 签名密钥分开），由本程序以 root 验签后才使用（upgrade.go）；
//   - 二进制与配置路径全部由 internal/layout 按实例名计算，参数不参与路径以外的任何拼接；
//   - 用切片方式执行，清空环境变量，绝不经过 shell；
//   - 永远不提供 restart、-g、-p 或任意 -c。
package wrapper

import (
	"errors"
	"regexp"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

var (
	// SyappBin 是数据面二进制的绝对路径（同机实例共用）。
	SyappBin = layout.System("").Bin()
	// ReleasesDir 是默认实例版本目录的根。
	ReleasesDir = layout.System("").ReleasesDir()
)

// ExitUsage 是参数非法时的退出码（与 sysexits.h 的 EX_USAGE 一致）。
const ExitUsage = 64

// ErrUsage 表示调用参数不被允许。
var ErrUsage = errors.New("usage: " + brand.Wrapper() + " test <revision> | reload | reopen | quit | upgrade-start <version> | upgrade-commit | upgrade-abort")

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$`)

var revisionRe = regexp.MustCompile(`^r[0-9]{6,12}$`)

// Command 是一次经过校验、可直接执行的调用。
type Command struct {
	Instance string // 可选的多实例名（匹配 ^[a-z0-9][a-z0-9_-]{0,31}$；默认单实例为空）
	Upgrade  string // 非空表示热升级动作：start | commit | abort（由 wrapper 自身执行，不调用外部命令）
	Version  string // upgrade-start 的目标版本
	Path     string
	Args     []string // 不含 argv[0]
	Env      []string
}

// SafeEnv 是执行 syapp 时使用的全部环境变量。
var SafeEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// Build 校验 argv（不含程序名）并返回要执行的命令。
// 任何不在白名单内的输入都返回 ErrUsage。
func Build(argv []string) (Command, error) {
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
	releasesDir := ReleasesDir
	currentConf := ""
	if instance != "" {
		l := layout.System(instance)
		releasesDir = l.ReleasesDir()
		currentConf = l.CurrentConf()
	}
	switch argv[0] {
	case "test":
		if len(argv) != 2 || !revisionRe.MatchString(argv[1]) {
			return Command{}, ErrUsage
		}
		conf := releasesDir + "/" + argv[1] + "/" + brand.ConfName()
		return Command{Instance: instance, Path: SyappBin, Args: []string{"-t", "-q", "-c", conf}, Env: SafeEnv}, nil
	case "reload", "reopen", "quit":
		if len(argv) != 1 {
			return Command{}, ErrUsage
		}
		args := []string{"-s", argv[0]}
		if currentConf != "" {
			args = []string{"-c", currentConf, "-s", argv[0]}
		}
		return Command{Instance: instance, Path: SyappBin, Args: args, Env: SafeEnv}, nil
	case "upgrade-start":
		if len(argv) != 2 || !versionRe.MatchString(argv[1]) {
			return Command{}, ErrUsage
		}
		return Command{Instance: instance, Upgrade: "start", Version: argv[1]}, nil
	case "upgrade-commit", "upgrade-abort":
		if len(argv) != 1 {
			return Command{}, ErrUsage
		}
		return Command{Instance: instance, Upgrade: strings.TrimPrefix(argv[0], "upgrade-")}, nil
	default:
		return Command{}, ErrUsage
	}
}
