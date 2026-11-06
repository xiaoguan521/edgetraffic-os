package wrapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// 运行时不使用 root（设计 0001 第 6.1 节方式 B，D5）：system 模式的 master 以 <品牌> 用户运行，
// systemd 只授予 CAP_NET_BIND_SERVICE。已经以 root 运行的 master（升级前启动）不能重启，
// 用与热升级相同的方式无中断切换：
//
//	共用软链临时指向 launcher → USR2 → 旧 master（root）exec launcher → launcher 降权为 <品牌> 用户、
//	只保留 CAP_NET_BIND_SERVICE（ambient），再 exec 真正的二进制；监听套接字随 exec 继承，连接不中断
//	→ 软链立即切回 → WINCH / QUIT 旧 master。
//
// launcher 是 root 所有的 <品牌>-instance 二进制：当它以数据面二进制的路径（basename 为品牌名）被执行时进入 Launch。
// 只有 root 执行的 Handover 会把软链指向它，并写入 root 所有的 versions/.handover.json 说明目标与身份。

const handoverFile = ".handover.json"

type handover struct {
	Target string `json:"target"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
}

// capNetBindService 是 CAP_NET_BIND_SERVICE 的编号。
const capNetBindService = 10

// ErrNotRoot：master 已经不是 root，无需切换。
var ErrNotRoot = errors.New("master is not running as root")

// ProcUID 返回进程的真实 uid（/proc/<pid>/status 的 Uid 行，任何用户可读）。
func ProcUID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "Uid:" {
			return strconv.Atoi(f[1])
		}
	}
	return -1, errors.New("no Uid line")
}

// procCapEff 返回进程的有效能力集（/proc/<pid>/status 的 CapEff）。
func procCapEff(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "CapEff:" {
			v, _ := strconv.ParseUint(f[1], 16, 64)
			return v
		}
	}
	return 0
}

// Handover 把以 root 运行的 master 无中断地切换为 uid/gid 运行（只保留 CAP_NET_BIND_SERVICE）。
// 由 root 执行（<品牌>-instance nonroot）；launcher 是 root 所有的 launcher 程序路径。
// 调用前，运行时目录、日志、缓存与版本库须已归 uid/gid 所有。
func Handover(p Paths, launcher string, uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return fail("handover target must not be root")
	}
	unlock, err := p.lockUpgrade()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(p.PidFile + ".oldbin"); err == nil {
		return fail("an upgrade is in progress (%s.oldbin exists): commit or abort it first", filepath.Base(p.PidFile))
	}
	oldPid, err := readPid(p.PidFile)
	if err != nil || !isMaster(oldPid) {
		return fail("master is not running")
	}
	if u, err := ProcUID(oldPid); err != nil {
		return err
	} else if u != 0 {
		return ErrNotRoot
	}
	real, err := p.current() // 首次：把普通文件收入 versions/<版本>，Bin 改为软链
	if err != nil {
		return err
	}
	hf := filepath.Join(p.Versions, handoverFile)
	b, _ := json.Marshal(handover{Target: real, UID: uid, GID: gid})
	_ = os.Remove(hf)
	if err := os.WriteFile(hf, b, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(hf) }()
	if err := p.swapLink(launcher); err != nil {
		return err
	}
	restored := false
	restore := func() {
		if !restored {
			_ = p.swapLink(real)
			restored = true
		}
	}
	defer restore()
	if err := signal(oldPid, syscall.SIGUSR2); err != nil {
		return err
	}
	var newPid int
	started := waitFor(p.Timeout, func() bool {
		if _, err := os.Stat(p.PidFile + ".oldbin"); err != nil {
			return false
		}
		n, err := readPid(p.PidFile)
		if err != nil || n == oldPid || !isMaster(n) {
			return false
		}
		newPid = n
		return true
	})
	restore() // 新 master 已经 exec 完成（或失败）：软链立即指回真正的二进制，之后的热升级照常
	if !started {
		return fail("new master did not start within %s; the root master keeps serving", p.Timeout)
	}
	if u, _ := ProcUID(newPid); u != uid || procCapEff(newPid)&(1<<capNetBindService) == 0 {
		// 身份或能力不对：让新 master 退出，旧 master 照常服务（还没有 WINCH，旧 worker 一直在）
		_ = signal(newPid, syscall.SIGQUIT)
		waitFor(p.Timeout, func() bool { return !alive(newPid) })
		return fail("new master runs as uid %d (capabilities %x), want uid %d with CAP_NET_BIND_SERVICE", u, procCapEff(newPid), uid)
	}
	if err := signal(oldPid, syscall.SIGWINCH); err != nil {
		return err
	}
	// 新 master 的 worker 起来后再让旧 master 退出
	waitFor(p.Timeout, func() bool { return workers(newPid) > 0 })
	if err := signal(oldPid, syscall.SIGQUIT); err != nil {
		return err
	}
	if !waitFor(p.Timeout, func() bool { return !alive(oldPid) }) {
		return fail("previous master did not exit within %s (it keeps draining connections)", p.Timeout)
	}
	return nil
}

// Launch 是 launcher 模式：argv0 是数据面二进制的路径（<前缀>/sbin/<品牌>，此刻软链指向 launcher）。
// 读取 root 所有的 versions/.handover.json，降权后 exec 目标二进制。不会返回；出错时退出（旧 master 继续服务）。
func Launch(argv0 string) {
	err := launch(argv0)
	fmt.Fprintln(os.Stderr, "launcher:", err)
	os.Exit(1)
}

func launch(argv0 string) error {
	if os.Geteuid() != 0 {
		return errors.New("launcher must be started by a root master")
	}
	versions := filepath.Join(filepath.Dir(filepath.Dir(argv0)), "versions")
	f, err := os.OpenFile(filepath.Join(versions, handoverFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// 版本库目录对运行用户可写：只信任 root 创建、他人不可写的说明文件（同一个 fd 先检查再读取，没有竞争窗口）
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != 0 || st.Mode().Perm()&0o022 != 0 || !st.Mode().IsRegular() {
		return errors.New("handover file must be a root-owned regular file not writable by others")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return err
	}
	var h handover
	if err := json.Unmarshal(b, &h); err != nil {
		return err
	}
	if h.UID <= 0 || h.GID <= 0 {
		return errors.New("refusing to keep root")
	}
	rel, err := filepath.Rel(versions, h.Target)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, "/") != 2 {
		return fmt.Errorf("target %s is not a binary in %s", h.Target, versions)
	}
	if ts, err := os.Lstat(h.Target); err != nil || !ts.Mode().IsRegular() {
		return fmt.Errorf("target %s is not a regular file", h.Target)
	}
	return dropAndExec(h, argv0)
}
