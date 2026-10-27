// Package wrapper 实现 syapp-wrapper：syapp-agent 唯一的提权入口（规格书 5.5）。
//
// 安全要点：
//   - 只接受固定的子命令 test / reload / reopen / quit；
//   - test 只接受一个 revision ID，且必须匹配 ^r[0-9]{6,12}$；
//   - 二进制与配置路径全部为编译期常量，参数不参与路径以外的任何拼接；
//   - 用切片方式执行，清空环境变量，绝不经过 shell；
//   - 永远不提供 restart、-g、-p 或任意 -c。
package wrapper

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	// SyappBin 是数据面二进制的绝对路径。
	SyappBin = "/opt/syapp/sbin/syapp"
	// ReleasesDir 是版本目录的根。
	ReleasesDir = "/opt/syapp/conf/releases"
)

// ExitUsage 是参数非法时的退出码（与 sysexits.h 的 EX_USAGE 一致）。
const ExitUsage = 64

// ErrUsage 表示调用参数不被允许。
var ErrUsage = errors.New("usage: syapp-wrapper test <revision> | reload | reopen | quit")

var revisionRe = regexp.MustCompile(`^r[0-9]{6,12}$`)

// Command 是一次经过校验、可直接执行的调用。
type Command struct {
	Path string
	Args []string // 不含 argv[0]
	Env  []string
}

// SafeEnv 是执行 syapp 时使用的全部环境变量。
var SafeEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// Build 校验 argv（不含程序名）并返回要执行的命令。
// 任何不在白名单内的输入都返回 ErrUsage。
func Build(argv []string) (Command, error) {
	if len(argv) == 0 {
		return Command{}, ErrUsage
	}
	switch argv[0] {
	case "test":
		if len(argv) != 2 || !revisionRe.MatchString(argv[1]) {
			return Command{}, ErrUsage
		}
		conf := fmt.Sprintf("%s/%s/syapp.conf", ReleasesDir, argv[1])
		return Command{Path: SyappBin, Args: []string{"-t", "-q", "-c", conf}, Env: SafeEnv}, nil
	case "reload", "reopen", "quit":
		if len(argv) != 1 {
			return Command{}, ErrUsage
		}
		return Command{Path: SyappBin, Args: []string{"-s", argv[0]}, Env: SafeEnv}, nil
	default:
		return Command{}, ErrUsage
	}
}
