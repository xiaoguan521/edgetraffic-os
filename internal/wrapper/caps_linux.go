package wrapper

import (
	"runtime"
	"syscall"
	"unsafe"
)

// 运行时不使用 root（设计 0001 D5）：wrapper 经 sudo 以数据面用户运行，但 `<品牌> -t` 会绑定配置中的端口来检查，
// 80/443 需要 CAP_NET_BIND_SERVICE。wrapper 文件带有该能力（只在 permitted 与 inheritable 集，不在 effective 集，
// 由 <品牌>-instance nonroot 设置，文件只允许数据面组执行）；RaiseBindCap 把它设为 ambient，
// 之后在本线程上启动的子进程（-t）获得这一项能力，其余能力一概没有。

// RaiseBindCap 在 wrapper 带有 CAP_NET_BIND_SERVICE 时把它设为 ambient，并把当前 goroutine 固定在本线程上
// （能力按线程保存，子进程从 fork 它的线程继承）。没有该能力时（user 模式、未设置文件能力）什么都不做。
func RaiseBindCap() error {
	runtime.LockOSThread()
	hdr := capHeader{version: linuxCapVersion3}
	var data [2]capData
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return e
	}
	if data[0].permitted&(1<<capNetBindService) == 0 {
		return nil
	}
	data[0].inheritable |= 1 << capNetBindService
	hdr = capHeader{version: linuxCapVersion3}
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return e
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientRaise, capNetBindService, 0, 0, 0); e != 0 {
		if e == syscall.EINVAL {
			return nil // 内核不支持 ambient 能力（4.3 之前，如 CentOS 7）：这类系统用兼容模式，wrapper 以 root 运行
		}
		return e
	}
	return nil
}

// BindCapXattr 返回写入 security.capability 扩展属性的内容（VFS_CAP_REVISION_2）：
// CAP_NET_BIND_SERVICE 在 permitted 与 inheritable 集，不设 effective 位。
func BindCapXattr() []byte {
	const vfsCapRevision2 = 0x02000000
	b := make([]byte, 20)
	le := func(off int, v uint32) {
		b[off], b[off+1], b[off+2], b[off+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	le(0, vfsCapRevision2)
	le(4, 1<<capNetBindService) // permitted（低 32 位）
	le(8, 1<<capNetBindService) // inheritable（低 32 位）
	return b
}
