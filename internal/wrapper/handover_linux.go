package wrapper

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	prSetKeepCaps     = 8
	prCapAmbient      = 47
	prCapAmbientRaise = 2
	linuxCapVersion3  = 0x20080522
)

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective, permitted, inheritable uint32
}

// dropAndExec 以 h.UID / h.GID 身份执行目标二进制，只保留 CAP_NET_BIND_SERVICE（作为 ambient 能力跨 exec 保留，
// 因此新 master 及其后热升级产生的 master 都能绑定 80/443，其余能力全部丢弃）。argv[0] 保持数据面二进制的路径，
// 以后的 USR2 仍按该路径执行软链指向的版本。
func dropAndExec(h handover, argv0 string) error {
	// 全部用当前线程上的原始系统调用：凭据与能力在内核中按线程保存，随后在同一线程上 exec，
	// 新进程映像只继承这个线程的身份（不依赖 Go 运行时的全线程 setuid，cgo 构建同样可用）
	runtime.LockOSThread()
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetKeepCaps, 1, 0); e != 0 {
		return e
	}
	groups := []uint32{uint32(h.GID)}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETGROUPS, 1, uintptr(unsafe.Pointer(&groups[0])), 0); e != 0 {
		return e
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETRESGID, uintptr(h.GID), uintptr(h.GID), uintptr(h.GID)); e != 0 {
		return e
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETRESUID, uintptr(h.UID), uintptr(h.UID), uintptr(h.UID)); e != 0 {
		return e
	}
	hdr := capHeader{version: linuxCapVersion3}
	var data [2]capData
	data[0] = capData{effective: 1 << capNetBindService, permitted: 1 << capNetBindService, inheritable: 1 << capNetBindService}
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return e
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientRaise, capNetBindService, 0, 0, 0); e != 0 {
		return e
	}
	args := append([]string{argv0}, os.Args[1:]...)
	return syscall.Exec(h.Target, args, os.Environ())
}
