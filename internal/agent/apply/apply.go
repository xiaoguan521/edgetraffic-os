// Package apply 实现边缘节点上的本地发布状态机（规格书 5.4）。
//
// 加锁 → 校验父版本 → 解包并校验清单 → syedge -t → 切换 current → reload →
// 确认加载（worker 全部更换、error.log 无 emerg/crit）→ 后置探活 → 完成；
// 任一步失败自动回滚到 previous。全程禁止 restart。
package apply

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// Status 与协议 CommandResult.Status 一一对应。
type Status string

const (
	Applied    Status = "APPLIED"
	Invalid    Status = "INVALID"
	Conflict   Status = "CONFLICT"
	RolledBack Status = "ROLLED_BACK"
	Degraded   Status = "DEGRADED"
	Rejected   Status = "REJECTED"
)

// Result 是一次发布或回滚的结果。
type Result struct {
	Status  Status
	Message string
	Stderr  string
}

// PostCheck 是发布后的本机 HTTP 探活。
type PostCheck struct {
	Host         string
	Path         string
	HTTPS        bool
	ExpectStatus []int
}

// Config 描述节点上的路径与提权方式。
type Config struct {
	ConfRoot     string          // /opt/syedge/conf
	PidFile      string          // /run/syedge/syedge.pid
	ErrorLog     string          // /var/log/syedge/error.log
	LockFile     string          // /run/syedge-agent/apply.lock
	Wrapper      []string        // 例如 ["sudo","-n","/usr/libexec/syedge/syedge-wrapper"]；root 运行时可直接写 wrapper 路径
	KeepRelease  int             // 保留的版本数，默认 20
	ConfirmWait  time.Duration   // reload 后等待 worker 更换的上限，默认 10s
	Workers      func(int) []int // 测试可注入 worker PID 查询（默认读取 /proc）
	HTTPPort     int             // 后置探活端口，默认 80 / 443
	HTTPSPort    int
	MinFreeBytes uint64                       // 解包前要求的最少可用磁盘字节（默认至少 16 MiB 或 2 倍包体积）
	FreeBytes    func(string) (uint64, error) // 测试可注入可用磁盘空间查询（默认 syscall.Statfs）
}

// Applier 执行发布。
type Applier struct{ C Config }

var revRe = regexp.MustCompile(`^r[0-9]{6,12}$`)

const (
	maxBundle     = 64 << 20
	maxFiles      = 4096
	manifestName  = "MANIFEST.json"
	maxErrLogScan = 1 << 20
)

type manifest struct {
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
	Digest   string            `json:"digest"`
}

func (a *Applier) releases() string { return filepath.Join(a.C.ConfRoot, "releases") }
func (a *Applier) link(name string) string {
	return filepath.Join(a.C.ConfRoot, name)
}

// Current 返回 current 软链指向的版本号；不存在时返回空串。
func (a *Applier) Current() string {
	t, err := os.Readlink(a.link("current"))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Apply 发布一个新版本。bundle 是渲染器生成的 tar.gz，digest 是其清单指纹。
func (a *Applier) Apply(ctx context.Context, rev, parent string, bundle []byte, digest string, checks []PostCheck) Result {
	if !revRe.MatchString(rev) || (parent != "" && !revRe.MatchString(parent)) {
		return Result{Status: Rejected, Message: "invalid revision id"}
	}
	unlock, err := a.lock()
	if err != nil {
		return lockFailed(err)
	}
	defer unlock()

	if cur := a.Current(); cur != parent {
		return Result{Status: Conflict, Message: fmt.Sprintf("node is at %q, expected parent %q", cur, parent)}
	}
	if err := a.unpack(rev, bundle, digest); err != nil {
		return Result{Status: Rejected, Message: "bundle rejected: " + err.Error()}
	}
	if out, err := a.wrapper(ctx, "test", rev); err != nil {
		_ = os.RemoveAll(filepath.Join(a.releases(), rev))
		return Result{Status: Invalid, Message: "configuration test failed", Stderr: out}
	}
	return a.switchTo(ctx, rev, true, checks)
}

// Restore 把被手工改动（漂移）的当前版本恢复为控制台下发的原样（规格书 5.6、12.7）：
// 用同一版本的干净副本原子替换版本目录，校验通过后 reload；失败时换回原目录。
func (a *Applier) Restore(ctx context.Context, rev string, bundle []byte, digest string, checks []PostCheck) Result {
	if !revRe.MatchString(rev) {
		return Result{Status: Rejected, Message: "invalid revision id"}
	}
	unlock, err := a.lock()
	if err != nil {
		return lockFailed(err)
	}
	defer unlock()
	if cur := a.Current(); cur != rev {
		return Result{Status: Conflict, Message: fmt.Sprintf("node is at %q, restore target is %q", cur, rev)}
	}
	backup, err := a.replaceDir(rev, bundle, digest)
	if err != nil {
		return Result{Status: Rejected, Message: "bundle rejected: " + err.Error()}
	}
	final := filepath.Join(a.releases(), rev)
	if out, err := a.wrapper(ctx, "test", rev); err != nil {
		_ = os.RemoveAll(final)
		_ = os.Rename(backup, final)
		return Result{Status: Invalid, Message: "configuration test failed", Stderr: out}
	}
	r := a.switchTo(ctx, rev, false, checks)
	if r.Status == Applied {
		r.Message = "restored to " + rev + "; manual changes kept in " + filepath.Base(backup)
	}
	return r
}

// replaceDir 用包中的干净副本替换版本目录，返回保留下来的旧目录（.drift-<版本>-<时间>）。
func (a *Applier) replaceDir(rev string, bundle []byte, digest string) (string, error) {
	if len(bundle) > maxBundle {
		return "", errors.New("bundle too large")
	}
	final := filepath.Join(a.releases(), rev)
	tmp := final + ".restore"
	if err := a.extractTo(tmp, rev, bundle, digest); err != nil {
		return "", err
	}
	backup := filepath.Join(a.releases(), fmt.Sprintf(".drift-%s-%d", rev, time.Now().UnixNano()))
	if err := os.Rename(final, backup); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Rename(backup, final)
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return backup, nil
}

// Rollback 把 current 切回本机已有的某个版本。
func (a *Applier) Rollback(ctx context.Context, rev string, checks []PostCheck) Result {
	if !revRe.MatchString(rev) {
		return Result{Status: Rejected, Message: "invalid revision id"}
	}
	unlock, err := a.lock()
	if err != nil {
		return lockFailed(err)
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(a.releases(), rev, brand.ConfName())); err != nil {
		return Result{Status: Rejected, Message: "revision not present on node"}
	}
	if out, err := a.wrapper(ctx, "test", rev); err != nil {
		return Result{Status: Invalid, Message: "configuration test failed", Stderr: out}
	}
	return a.switchTo(ctx, rev, false, checks)
}

// switchTo 切换 current、reload、确认并探活；失败时回到切换前的版本。
func (a *Applier) switchTo(ctx context.Context, rev string, isNew bool, checks []PostCheck) Result {
	prevTarget, _ := os.Readlink(a.link("current"))
	before, err := a.snapshot()
	if err != nil {
		return Result{Status: Degraded, Message: brand.ID() + " is not running: " + err.Error()}
	}
	if err := a.setLink("current", filepath.Join("releases", rev)); err != nil {
		return Result{Status: Degraded, Message: "switch failed: " + err.Error()}
	}
	fail := func(reason, stderr string) Result {
		if prevTarget == "" {
			return Result{Status: Degraded, Message: reason + "; no previous revision to roll back to", Stderr: stderr}
		}
		_ = a.setLink("current", prevTarget)
		// master 可能仍在处理失败的 reload（例如 bind 失败会重试约 2.5s 并持续写日志），
		// 等错误日志安静下来再回滚，避免把上一次的错误算到回滚头上。
		a.waitLogQuiet(ctx, 1500*time.Millisecond, 15*time.Second)
		b2, err := a.snapshot()
		if err == nil {
			if out, err := a.wrapper(ctx, "reload"); err == nil {
				cerr := a.confirm(ctx, b2)
				if cerr == nil {
					return Result{Status: RolledBack, Message: reason + "; rolled back to " + filepath.Base(prevTarget), Stderr: stderr}
				}
				stderr += "\nrollback: " + cerr.Error()
			} else {
				stderr += "\n" + out
			}
		}
		return Result{Status: Degraded, Message: reason + "; rollback also failed", Stderr: stderr}
	}
	if out, err := a.wrapper(ctx, "reload"); err != nil {
		return fail("reload failed", out)
	}
	if err := a.confirm(ctx, before); err != nil {
		return fail("reload not confirmed: "+err.Error(), "")
	}
	if err := a.postChecks(ctx, checks); err != nil {
		return fail("post check failed: "+err.Error(), "")
	}
	if prevTarget != "" && filepath.Base(prevTarget) != rev {
		_ = a.setLink("previous", prevTarget)
	}
	if isNew {
		a.cleanup()
	}
	return Result{Status: Applied, Message: "applied " + rev}
}

// lockFailed 区分“另一次发布正在进行”（锁被占用）与“无法创建锁”（运行时目录缺失或无权限）：
// 后者不是并发冲突，报告为 REJECTED 并说明原因，避免误导排查。
func lockFailed(err error) Result {
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return Result{Status: Conflict, Message: "another apply is running"}
	}
	return Result{Status: Rejected, Message: "cannot take the apply lock (agent runtime directory missing or not writable; " +
		"it is created by the service unit or systemd-tmpfiles): " + err.Error()}
}

func (a *Applier) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(a.C.LockFile), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.C.LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// setLink 原子地把 name 指向 target（先建临时软链再 rename）。
func (a *Applier) setLink(name, target string) error {
	tmp := a.link("." + name + ".tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, a.link(name))
}

func (a *Applier) wrapper(ctx context.Context, args ...string) (string, error) {
	if len(a.C.Wrapper) == 0 {
		return "", errors.New("wrapper not configured")
	}
	argv := append(append([]string(nil), a.C.Wrapper[1:]...), args...)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, a.C.Wrapper[0], argv...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// unpack 解包到 releases/<rev>.tmp，逐文件校验清单后改名为 releases/<rev>。
func (a *Applier) unpack(rev string, bundle []byte, digest string) error {
	if len(bundle) > maxBundle {
		return errors.New("bundle too large")
	}
	final := filepath.Join(a.releases(), rev)
	if _, err := os.Stat(final); err == nil {
		// 同一版本重复下发：校验现有目录后复用
		if m, err := readManifest(final); err == nil && m.Digest == digest {
			return verifyDir(final, m)
		}
		if err := os.RemoveAll(final); err != nil {
			return err
		}
	}
	tmp := final + ".tmp"
	if err := a.extractTo(tmp, rev, bundle, digest); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// extractTo 把包解到 tmp 并按清单逐文件校验；失败时清理 tmp。
func (a *Applier) extractTo(tmp, rev string, bundle []byte, digest string) error {
	if err := a.checkFreeSpace(filepath.Dir(tmp), uint64(len(bundle))); err != nil {
		return err
	}
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

	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if n++; n > maxFiles {
			return errors.New("too many files")
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s: only regular files allowed", h.Name)
		}
		clean := path.Clean(h.Name)
		if clean != h.Name || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			return fmt.Errorf("%s: unsafe path", h.Name)
		}
		p := filepath.Join(tmp, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(clean, ".key") {
			mode = 0o640
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(tr, maxBundle))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	m, err := readManifest(tmp)
	if err != nil {
		return err
	}
	if m.Revision != rev {
		return fmt.Errorf("manifest revision %q != %q", m.Revision, rev)
	}
	if m.Digest != digest {
		return errors.New("manifest digest mismatch")
	}
	if err := verifyDir(tmp, m); err != nil {
		return err
	}
	ok = true
	return nil
}

func (a *Applier) checkFreeSpace(dir string, bundleBytes uint64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	need := a.C.MinFreeBytes
	if need == 0 {
		need = 16 << 20
		if two := 2 * bundleBytes; two > need {
			need = two
		}
	}
	var avail uint64
	if a.C.FreeBytes != nil {
		var err error
		avail, err = a.C.FreeBytes(dir)
		if err != nil {
			return err
		}
	} else {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return err
		}
		//nolint:gosec // Bsize is always positive for mounted filesystems
		avail = st.Bavail * uint64(st.Bsize)
	}
	if avail < need {
		return fmt.Errorf("insufficient disk space: %d bytes available, need at least %d bytes", avail, need)
	}
	return nil
}

func readManifest(dir string) (*manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// verifyDir 校验目录内容与清单完全一致（不多不少），并重算整体指纹。
func verifyDir(dir string, m *manifest) error {
	seen := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == manifestName {
			return nil
		}
		want, ok := m.Files[rel]
		if !ok {
			return fmt.Errorf("%s: not in manifest", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("%s: checksum mismatch", rel)
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(m.Files) {
		return fmt.Errorf("manifest lists %d files, found %d", len(m.Files), seen)
	}
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	all := sha256.New()
	for _, n := range names {
		_, _ = fmt.Fprintf(all, "%s %s\n", m.Files[n], n)
	}
	if "sha256:"+hex.EncodeToString(all.Sum(nil)) != m.Digest {
		return errors.New("manifest digest does not match file list")
	}
	return nil
}

// CurrentFiles 读取当前版本目录中的全部文件（授权只读时用于核对“只有证书变化”）。
func (a *Applier) CurrentFiles() (map[string][]byte, error) {
	dir, err := filepath.EvalSymlinks(a.link("current"))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// TreeDigest 重新计算 current 目录的指纹，供漂移检测使用（规格书 5.6）。
func (a *Applier) TreeDigest() (string, error) {
	dir, err := filepath.EvalSymlinks(a.link("current"))
	if err != nil {
		return "", err
	}
	m, err := readManifest(dir)
	if err != nil {
		return "drifted:" + err.Error(), nil
	}
	if err := verifyDir(dir, m); err != nil {
		return "drifted:" + err.Error(), nil
	}
	return m.Digest, nil
}

type procSnapshot struct {
	master  int
	workers map[int]bool
	logOff  int64
}

func (a *Applier) snapshot() (procSnapshot, error) {
	b, err := os.ReadFile(a.C.PidFile)
	if err != nil {
		return procSnapshot{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return procSnapshot{}, err
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return procSnapshot{}, fmt.Errorf("master %d: %w", pid, err)
	}
	s := procSnapshot{master: pid, workers: a.workersOf(pid)}
	if fi, err := os.Stat(a.C.ErrorLog); err == nil {
		s.logOff = fi.Size()
	}
	return s, nil
}

func (a *Applier) workersOf(parent int) map[int]bool {
	if a.C.Workers != nil {
		res := map[int]bool{}
		for _, pid := range a.C.Workers(parent) {
			res[pid] = true
		}
		return res
	}
	return childPIDs(parent)
}

// childPIDs 从 /proc 读取某进程的直接子进程。
func childPIDs(parent int) map[int]bool {
	res := map[int]bool{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// /proc/<pid>/stat: pid (comm) state ppid ...
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) > 1 {
			if ppid, _ := strconv.Atoi(f[1]); ppid == parent {
				res[pid] = true
			}
		}
	}
	return res
}

// badLog：reload 失败的迹象。init_by_lua 出错（例如自定义 Lua 脚本的语法错误，syedge -t 不执行 init 阶段）只记为 [error]，
// master 不加载新配置、继续以当前配置服务；按此识别可以立即回滚，并把脚本与行号带进发布记录。
var badLog = regexp.MustCompile(`\[(emerg|crit)\]|init_by_lua(_block)? error`)

// confirm 等待新一代 worker 全部起来，并检查 reload 期间的 error.log。
func (a *Applier) confirm(ctx context.Context, before procSnapshot) error {
	wait := a.C.ConfirmWait
	if wait == 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		now, err := a.snapshot()
		if err != nil {
			return err
		}
		if now.master != before.master {
			return fmt.Errorf("master pid changed %d -> %d", before.master, now.master)
		}
		if line := a.newErrors(before.logOff); line != "" {
			return fmt.Errorf("error log: %s", line)
		}
		fresh := len(now.workers) > 0
		for pid := range now.workers {
			if before.workers[pid] {
				fresh = false
				break
			}
		}
		if fresh {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("workers were not replaced in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitLogQuiet 等待 error.log 在 quiet 时长内不再增长，最多等 max。
func (a *Applier) waitLogQuiet(ctx context.Context, quiet, max time.Duration) {
	size := func() int64 {
		if fi, err := os.Stat(a.C.ErrorLog); err == nil {
			return fi.Size()
		}
		return 0
	}
	last, since, end := size(), time.Now(), time.Now().Add(max)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		if s := size(); s != last {
			last, since = s, time.Now()
		} else if time.Since(since) >= quiet {
			return
		}
	}
}

func (a *Applier) newErrors(off int64) string {
	f, err := os.Open(a.C.ErrorLog)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, maxErrLogScan))
	for _, line := range strings.Split(string(b), "\n") {
		if badLog.MatchString(line) {
			return line
		}
	}
	return ""
}

func (a *Applier) postChecks(ctx context.Context, checks []PostCheck) error {
	for _, c := range checks {
		scheme, port := "http", a.C.HTTPPort
		if c.HTTPS {
			scheme, port = "https", a.C.HTTPSPort
		}
		if port == 0 {
			port = 80
			if c.HTTPS {
				port = 443
			}
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		tr := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{ServerName: c.Host, InsecureSkipVerify: true}, //nolint:gosec // 本机探活，只关心可达与状态码
		}
		cl := &http.Client{Transport: tr, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+c.Host+c.Path, nil)
		resp, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s%s: %v", scheme, c.Host, c.Path, err)
		}
		_ = resp.Body.Close()
		okStatus := len(c.ExpectStatus) == 0 && resp.StatusCode < 500
		for _, s := range c.ExpectStatus {
			if s == resp.StatusCode {
				okStatus = true
			}
		}
		if !okStatus {
			return fmt.Errorf("%s %s%s: status %d", scheme, c.Host, c.Path, resp.StatusCode)
		}
	}
	return nil
}

// cleanup 只保留最近 KeepRelease 个版本，且永不删除 current / previous。
func (a *Applier) cleanup() {
	keep := a.C.KeepRelease
	if keep <= 0 {
		keep = 20
	}
	ents, err := os.ReadDir(a.releases())
	if err != nil {
		return
	}
	var revs []string
	for _, e := range ents {
		if e.IsDir() && revRe.MatchString(e.Name()) {
			revs = append(revs, e.Name())
		}
	}
	sort.Strings(revs)
	protect := map[string]bool{}
	for _, l := range []string{"current", "previous"} {
		if t, err := os.Readlink(a.link(l)); err == nil {
			protect[filepath.Base(t)] = true
		}
	}
	for len(revs) > keep {
		r := revs[0]
		revs = revs[1:]
		if !protect[r] {
			_ = os.RemoveAll(filepath.Join(a.releases(), r))
		}
	}
	// 漂移恢复留下的备份目录只保留最近 5 个
	var drifts []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".drift-") {
			drifts = append(drifts, e.Name())
		}
	}
	sort.Slice(drifts, func(i, j int) bool { return driftTime(drifts[i]) < driftTime(drifts[j]) })
	for len(drifts) > 5 {
		_ = os.RemoveAll(filepath.Join(a.releases(), drifts[0]))
		drifts = drifts[1:]
	}
}

func driftTime(name string) int64 {
	i := strings.LastIndexByte(name, '-')
	n, _ := strconv.ParseInt(name[i+1:], 10, 64)
	return n
}

// Bootstrap 在全新节点上写入初始版本并建立 current 软链（由安装脚本以 root 调用）。
func (a *Applier) Bootstrap(rev string, bundle []byte, digest string) error {
	if a.Current() != "" {
		return nil
	}
	if err := os.MkdirAll(a.releases(), 0o755); err != nil {
		return err
	}
	if err := a.unpack(rev, bundle, digest); err != nil {
		return err
	}
	return a.setLink("current", filepath.Join("releases", rev))
}

// PostChecks 执行探活（数据面热升级后使用，规格书 12.6）。
func (a *Applier) PostChecks(ctx context.Context, checks []PostCheck) error {
	return a.postChecks(ctx, checks)
}
