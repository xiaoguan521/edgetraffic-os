package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/wrapper"
)

// 运行时不使用 root（设计 0001 第 6.1 节方式 B，D5）：system 模式的 master 以数据面用户运行，systemd 只授予
// CAP_NET_BIND_SERVICE；wrapper 经 sudo 以数据面用户（不是 root）运行。

// defaultUnitNoFile 是服务单元的 LimitNOFILE；档案的 nofile 更大时 add 写单元附加配置。
const defaultUnitNoFile = 65535

// ErrHandoverUnavailable：Env 没有提供切换函数（测试）。
var ErrHandoverUnavailable = errors.New("handover not available")

// Nonroot 让本机（system 模式）全部实例在运行时不使用 root，可重复执行：
//  1. 运行时目录、日志、缓存、版本库、二进制目录与模块目录交给数据面用户；
//  2. 重写具名实例的 sudoers，wrapper 以数据面用户运行；
//  3. 仍以 root 运行的 master（升级前启动）在不中断连接的情况下切换为数据面用户（与二进制热升级同一机制）；
//  4. 重启 agent（只重启 agent 进程），使其以数据面用户调用 wrapper。
//
// 软件包安装与升级时自动执行；数据面不会停止或重启。
func (e Env) Nonroot() error {
	if e.user() {
		return errors.New("user 模式本来就不使用 root，无需切换")
	}
	insts, err := e.installed()
	if err != nil {
		return err
	}
	dp := layout.WorkerUser()
	owner := dp + ":" + dp
	def := layout.System("")
	e.say("== 运行时目录交给 %s", dp)
	// 共用：版本库（含升级状态文件）递归；二进制目录与模块目录只改目录本身（wrapper 在其中切换软链、安装新版本，
	// 已安装的文件仍由 root 拥有）
	for _, c := range [][]string{
		{"-R", owner, def.Versions()},
		{owner, filepath.Dir(def.Bin())},
		{owner, def.Modules()},
	} {
		if _, err := os.Stat(e.path(c[len(c)-1])); err != nil {
			if err := os.MkdirAll(e.path(c[len(c)-1]), 0o755); err != nil {
				return err
			}
		}
		if err := e.Run("chown", c...); err != nil {
			return err
		}
	}
	if err := e.wrapperCaps(); err != nil {
		return err
	}
	for _, in := range insts {
		l := layout.System(in.Name)
		for _, d := range []string{l.RunDir(), l.LogDir(), l.CacheDir()} {
			if err := os.MkdirAll(e.path(d), 0o755); err != nil {
				return err
			}
			if err := e.Run("chown", "-R", owner, d); err != nil {
				return err
			}
		}
		if in.Name != "" {
			if err := e.writeSudoers(l); err != nil {
				return err
			}
		}
	}
	e.say("== 切换仍以 root 运行的 master（连接不中断）")
	var failed []string
	for _, in := range insts {
		switch err := e.handover(in.Name); {
		case err == nil:
			e.say("%s：已切换为 %s 运行", label(in.Name), dp)
		case errors.Is(err, wrapper.ErrNotRoot):
			e.say("%s：已经不是 root", label(in.Name))
		case errors.Is(err, errNotRunning):
			e.say("%s：未运行（启动时直接以 %s 运行）", label(in.Name), dp)
		default:
			e.say("%s：切换失败：%v", label(in.Name), err)
			failed = append(failed, label(in.Name))
		}
	}
	// 切换以 root 执行，会在版本库中留下 root 所有的文件（升级锁、首次收入的原二进制）：再交给数据面用户一次
	if err := e.Run("chown", "-R", owner, def.Versions()); err != nil {
		return err
	}
	// agent 以新的方式调用 wrapper：重启 agent 进程（数据面不受影响）；未启用的单元不会被启动
	units := []string{def.AgentUnit()}
	for _, in := range insts {
		if in.Name != "" {
			units = append(units, layout.System(in.Name).AgentUnit())
		}
	}
	_ = e.Run("systemctl", append([]string{"try-restart"}, units...)...)
	if len(failed) > 0 {
		return fmt.Errorf("%v 仍以 root 运行：请检查后重新执行 %s-instance nonroot（数据面一直在服务）", failed, brand.ID())
	}
	return nil
}

var errNotRunning = errors.New("master not running")

// wrapperCaps：wrapper 以数据面用户运行，`-t` 需要绑定 80/443，文件带 CAP_NET_BIND_SERVICE（permitted 与 inheritable，
// 不在 effective）；只有数据面组可以执行。软件包每次升级都会换掉文件，因此每次 nonroot / add 都重新设置。
func (e Env) wrapperCaps() error {
	w := layout.System("").WrapperBin()
	if _, err := os.Stat(e.path(w)); err != nil {
		return nil // 只装了数据面软件包
	}
	if err := e.Run("chown", "root:"+layout.WorkerUser(), w); err != nil {
		return err
	}
	if err := e.Run("chmod", "0750", w); err != nil {
		return err
	}
	if e.SetCaps == nil {
		return ErrHandoverUnavailable
	}
	if err := e.SetCaps(e.path(w), wrapper.BindCapXattr()); err != nil {
		return fmt.Errorf("给 %s 设置 CAP_NET_BIND_SERVICE: %w", w, err)
	}
	return nil
}

func (e Env) handover(name string) error {
	l := layout.System(name)
	if b, err := os.ReadFile(e.path(l.PidFile())); err != nil || len(b) == 0 {
		return errNotRunning
	}
	if e.Handover == nil {
		return ErrHandoverUnavailable
	}
	return e.Handover(name)
}

// writeSudoers 写入具名实例的 sudoers（visudo 校验通过才生效）。
func (e Env) writeSudoers(l layout.Layout) error {
	sp := e.path(l.SudoersFile())
	if err := os.MkdirAll(filepath.Dir(sp), 0o750); err != nil {
		return err
	}
	tmp := sp + ".new"
	if err := writeFile(tmp, []byte(Sudoers(l)), 0o440); err != nil {
		return err
	}
	if err := e.Run("visudo", "-cf", l.SudoersFile()+".new"); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sudoers 校验失败: %w", err)
	}
	return os.Rename(tmp, sp)
}
