package wrapper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

// 数据面二进制热升级（规格书 12.6）：全程不 restart、不 stop syapp。
//
//	upgrade-start <版本>  验签 → 解压到 versions/<版本> → 新二进制 -t → 切换软链 → USR2 给当前 master
//	                     → 等新 master 起来 → WINCH 给旧 master（syapp.pid.oldbin），旧 worker 优雅退出
//	upgrade-commit       探活通过后：QUIT 给旧 master
//	upgrade-abort        探活失败：HUP 给旧 master 拉起旧 worker，QUIT 给新 master，软链切回
//
// 升级包由 agent 放在 Incoming 目录（agent 可写），wrapper 以 root 读入内存后验签、解压，只用验过签的字节。

// Paths 是升级涉及的路径与参数（生产值见 DefaultPaths；测试可替换）。
type Paths struct {
	Bin      string // 数据面二进制路径（首次升级后为指向 versions/<版本>/sbin/syapp 的软链）
	Modules  string // 动态模块目录（syapp-lua 附加包，见 lua.go）；为空表示不处理附加包
	Versions string // 已安装版本目录（root 所有）
	Incoming string // 升级包投放目录（agent 可写）
	PidFile  string
	Conf     string
	Keys     map[string]ed25519.PublicKey
	Arch     string
	Timeout  time.Duration
}

// DefaultPaths 是生产环境的路径。
func DefaultPaths() Paths {
	return DefaultPathsForInstance("")
}

// DefaultPathsForInstance 返回指定实例（空串表示默认单实例）的热升级路径。
func DefaultPathsForInstance(instance string) Paths {
	l := layout.System(instance)
	return Paths{Bin: l.Bin(), Modules: l.Modules(), Versions: l.Versions(), Incoming: l.UpgradeIncoming(),
		PidFile: l.PidFile(), Conf: l.CurrentConf(), Keys: pkgsig.Keys(), Arch: runtime.GOARCH, Timeout: 15 * time.Second}
}

const (
	maxPackage          = 128 << 20
	maxEntry            = 96 << 20
	minUpgradeFreeBytes = 64 << 20
	shaFile             = ".sha256"
	stateFile           = "state.json"
	upgradeLockFile     = ".upgrade.lock"
)

var (
	ErrUpgrade = errors.New("upgrade failed")
	entryRe    = regexp.MustCompile(`^(` + regexp.QuoteMeta(binEntry) + `|modules/[a-z0-9_]+\.so|VERSION|NOTICE)$`)
	verOutRe   = regexp.MustCompile(regexp.QuoteMeta(brand.VersionPrefix()) + `([0-9]+\.[0-9]+\.[0-9]+)`)
	// binEntry 是升级包与版本目录中数据面二进制的相对路径。
	binEntry = "sbin/" + brand.ID()
)

// State 记录上一次升级，用于中止时切回与一键回退。
type State struct {
	Previous string    `json:"previous"`           // 升级前的二进制路径
	PrevLua  string    `json:"prev_lua,omitempty"` // 升级前 syapp-lua 软链的目标（未装附加包为空）
	Target   string    `json:"target"`             // 目标版本
	Phase    string    `json:"phase"`              // started | committed | aborted
	At       time.Time `json:"at"`
}

func fail(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUpgrade, fmt.Sprintf(format, a...))
}

// readNoFollow 读取投放目录中的文件：拒绝软链与非普通文件（投放目录对 agent 可写）。
func readNoFollow(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fail("%s is not a regular file within the size limit", filepath.Base(path))
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

func fileSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// installed 报告 versions/<ver> 是否完整安装且二进制未被改动。
func (p Paths) installed(ver string) bool {
	dir := filepath.Join(p.Versions, ver)
	want, err := os.ReadFile(filepath.Join(dir, shaFile))
	return err == nil && strings.TrimSpace(string(want)) == fileSHA(filepath.Join(dir, binEntry))
}

// install 校验签名并解压升级包到 versions/<ver>。
func (p Paths) install(ver string) error {
	base := filepath.Join(p.Incoming, brand.ID()+"-"+ver+".tar.gz")
	data, err := readNoFollow(base, maxPackage)
	if err != nil {
		return fail("read package: %v", err)
	}
	sb, err := readNoFollow(base+".sig", 64<<10)
	if err != nil {
		return fail("read signature: %v", err)
	}
	sig, err := pkgsig.Parse(sb)
	if err != nil {
		return err
	}
	if sig.Name != brand.ID() || sig.Version != ver || sig.Arch != p.Arch {
		return fail("signature is for %s %s %s, want %s %s %s", sig.Name, sig.Version, sig.Arch, brand.ID(), ver, p.Arch)
	}
	if err := pkgsig.Verify(sig, data, p.Keys); err != nil {
		return err
	}
	if err := checkUpgradeDiskSpace(p.Versions); err != nil {
		return err
	}
	tmp := filepath.Join(p.Versions, "."+ver+".tmp")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fail("bad package: %v", err)
	}
	tr := tar.NewReader(zr)
	got := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("bad package: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || !entryRe.MatchString(name) || h.Size > maxEntry {
			return fail("package entry %q not allowed", h.Name)
		}
		dst := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if name == binEntry {
			mode, got = 0o755, true
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(tr, maxEntry))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if !got {
		return fail("package has no %s", binEntry)
	}
	if err := os.WriteFile(filepath.Join(tmp, shaFile), []byte(fileSHA(filepath.Join(tmp, binEntry))+"\n"), 0o644); err != nil {
		return err
	}
	dir := filepath.Join(p.Versions, ver)
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	ok = true
	return nil
}

func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fail("bad pid file %s", path)
	}
	return pid, nil
}

// isMaster 确认 pid 是 syapp 的 master 进程（防止 pid 文件被篡改后向任意进程发信号）。
func isMaster(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && strings.HasPrefix(string(b), brand.MasterTitle())
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// workers 统计某个 master 正在服务的 worker 子进程数。
func workers(master int) int { return len(workerPids(master)) }

// workerPids 返回某个 master 正在服务的 worker 子进程（不含正在优雅退出的）。
func workerPids(master int) map[int]bool {
	out := map[int]bool{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// /proc/<pid>/stat：pid (comm) state ppid ...；comm 可能含空格，从最后一个 ')' 之后解析
		i := bytes.LastIndexByte(st, ')')
		f := strings.Fields(string(st[i+1:]))
		if len(f) < 2 || f[1] != strconv.Itoa(master) {
			continue
		}
		// 收到 WINCH 正在优雅退出的 worker 标题为 "worker process is shutting down"，不算在内
		if cl, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && strings.HasPrefix(string(cl), brand.WorkerTitle()) &&
			!strings.Contains(string(cl), "shutting down") {
			out[pid] = true
		}
	}
	return out
}

func signal(pid int, sig syscall.Signal) error {
	if !isMaster(pid) {
		return fail("pid %d is not a %s master process", pid, brand.ID())
	}
	return syscall.Kill(pid, sig)
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// swapLink 原子地把 Bin 指向 target。
func (p Paths) swapLink(target string) error {
	tmp := p.Bin + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, p.Bin)
}

// current 返回 Bin 当前指向的二进制；Bin 还是普通文件（首次升级）时，先把它移入 versions/<其版本号>。
func (p Paths) current() (string, error) {
	st, err := os.Lstat(p.Bin)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return os.Readlink(p.Bin)
	}
	out, err := exec.Command(p.Bin, "-v").CombinedOutput()
	m := verOutRe.FindSubmatch(out)
	if err != nil || m == nil {
		return "", fail("cannot read the current version: %v", err)
	}
	ver := string(m[1])
	dir := filepath.Join(p.Versions, ver)
	if !p.installed(ver) {
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(filepath.Join(dir, "sbin"), 0o755); err != nil {
			return "", err
		}
		b, err := os.ReadFile(p.Bin)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, binEntry), b, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, shaFile), []byte(fileSHA(p.Bin)+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	target := filepath.Join(dir, binEntry)
	if err := p.swapLink(target); err != nil {
		return "", err
	}
	return target, nil
}

func (p Paths) saveState(s State) error {
	s.At = time.Now().UTC()
	b, _ := json.Marshal(s)
	return os.WriteFile(filepath.Join(p.Versions, stateFile), b, 0o644)
}

func (p Paths) loadState() (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(p.Versions, stateFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func checkUpgradeDiskSpace(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	//nolint:gosec // Bsize is always positive for mounted filesystems
	avail := st.Bavail * uint64(st.Bsize)
	if avail < minUpgradeFreeBytes {
		return fail("insufficient disk space in %s: %d bytes available, need at least %d bytes", dir, avail, minUpgradeFreeBytes)
	}
	return nil
}

// lockUpgrade 获取宿主机级版本库互斥锁（防止同机多实例同时修改 /opt/syapp/versions 与软链）。
func (p Paths) lockUpgrade() (func(), error) {
	if err := os.MkdirAll(p.Versions, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.Versions, upgradeLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fail("another instance is upgrading binaries on this host")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// UpgradeStart 执行到“新旧 master 共存、只剩新 worker 接流量”为止。
func UpgradeStart(p Paths, ver string) error {
	if !pkgsig.VersionRe.MatchString(ver) {
		return ErrUsage
	}
	unlock, err := p.lockUpgrade()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(p.PidFile + ".oldbin"); err == nil {
		return fail("another upgrade is in progress (%s.oldbin exists): commit or abort it first", filepath.Base(p.PidFile))
	}
	oldPid, err := readPid(p.PidFile)
	if err != nil || !isMaster(oldPid) {
		return fail("%s master is not running", brand.ID())
	}
	if !p.installed(ver) {
		if err := p.install(ver); err != nil {
			return err
		}
	}
	newBin := filepath.Join(p.Versions, ver, binEntry)
	// 装有 syapp-lua 时，先把模块软链切到目标版本（新二进制 -t 与新 master 都会加载它）；后续任一步失败都切回
	prevLua, err := p.prepareLua(ver, newBin)
	if err != nil {
		return err
	}
	restoreLua := func() {
		if prevLua != "" {
			_ = p.swapLua(prevLua)
		}
	}
	if out, err := exec.Command(newBin, "-t", "-q", "-c", p.Conf).CombinedOutput(); err != nil {
		restoreLua()
		return fail("new binary rejects the current configuration: %s", strings.TrimSpace(string(out)))
	}
	prev, err := p.current()
	if err != nil {
		restoreLua()
		return err
	}
	if prev == newBin {
		restoreLua()
		return fail("version %s is already running", ver)
	}
	if err := p.saveState(State{Previous: prev, PrevLua: prevLua, Target: ver, Phase: "started"}); err != nil {
		restoreLua()
		return err
	}
	if err := p.swapLink(newBin); err != nil {
		restoreLua()
		return err
	}
	if err := signal(oldPid, syscall.SIGUSR2); err != nil {
		_ = p.swapLink(prev)
		restoreLua()
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
	if !started {
		// 新 master 没有起来：旧 master 仍在服务，切回软链即可（syapp 会自行恢复 pid 文件）
		_ = p.swapLink(prev)
		restoreLua()
		_ = p.saveState(State{Previous: prev, PrevLua: prevLua, Target: ver, Phase: "aborted"})
		return fail("new master did not start within %s; switched back to the previous binary", p.Timeout)
	}
	if err := signal(oldPid, syscall.SIGWINCH); err != nil {
		return err
	}
	_ = newPid
	return nil
}

// UpgradeCommit 让旧 master 退出，完成升级。
func UpgradeCommit(p Paths) error {
	unlock, err := p.lockUpgrade()
	if err != nil {
		return err
	}
	defer unlock()
	oldPid, err := readPid(p.PidFile + ".oldbin")
	if err != nil {
		return fail("no upgrade in progress")
	}
	if err := signal(oldPid, syscall.SIGQUIT); err != nil {
		return err
	}
	waitFor(p.Timeout, func() bool { return !alive(oldPid) })
	st, _ := p.loadState()
	st.Phase = "committed"
	return p.saveState(st)
}

// UpgradeAbort 恢复旧版本：旧 master 重新拉起 worker，新 master 退出，软链切回。
func UpgradeAbort(p Paths) error {
	unlock, err := p.lockUpgrade()
	if err != nil {
		return err
	}
	defer unlock()
	oldPid, err := readPid(p.PidFile + ".oldbin")
	if err != nil {
		return fail("no upgrade in progress")
	}
	newPid, err := readPid(p.PidFile)
	if err != nil {
		return err
	}
	before := workerPids(oldPid) // 收到 WINCH 后可能还没退完的旧 worker，不能算作“已重新拉起”
	// HUP 会让旧 master 重新加载配置（含 load_module）：先把 syapp-lua 软链切回旧版本
	if st, err := p.loadState(); err == nil && st.PrevLua != "" {
		if err := p.swapLua(st.PrevLua); err != nil {
			return err
		}
	}
	if err := signal(oldPid, syscall.SIGHUP); err != nil {
		return err
	}
	// 等旧 master 的 worker 重新起来后再让新 master 退出，保证任何时刻都有 worker 接流量。
	// 旧 worker 尚未退完时 master 可能还在处理上一个信号，每秒补发一次 HUP（重复 HUP 无副作用）。
	last := time.Now()
	if !waitFor(p.Timeout, func() bool {
		for pid := range workerPids(oldPid) {
			if !before[pid] {
				return true
			}
		}
		if time.Since(last) > time.Second {
			_ = signal(oldPid, syscall.SIGHUP)
			last = time.Now()
		}
		return false
	}) {
		return fail("previous master did not start workers within %s; the new version keeps serving", p.Timeout)
	}
	if newPid != oldPid && isMaster(newPid) {
		if err := signal(newPid, syscall.SIGQUIT); err != nil {
			return err
		}
	}
	// 新 master 退出后，旧 master 会把 .oldbin 改回 pid 文件
	if !waitFor(p.Timeout, func() bool {
		n, err := readPid(p.PidFile)
		_, oe := os.Stat(p.PidFile + ".oldbin")
		return err == nil && n == oldPid && oe != nil
	}) {
		return fail("previous master did not take over within %s", p.Timeout)
	}
	st, err := p.loadState()
	if err == nil && st.Previous != "" {
		if err := p.swapLink(st.Previous); err != nil {
			return err
		}
	}
	st.Phase = "aborted"
	return p.saveState(st)
}
