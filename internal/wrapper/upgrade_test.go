package wrapper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

func makePackage(t *testing.T, bin []byte, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "sbin/syedge", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(bin)
	for n, b := range extra {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(b))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func drop(t *testing.T, p Paths, priv ed25519.PrivateKey, ver string, pkg []byte) {
	t.Helper()
	sig, err := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge", ver, p.Arch, "standard", pkg)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(sig)
	base := filepath.Join(p.Incoming, "syedge-"+ver+".tar.gz")
	if err := os.WriteFile(base, pkg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".sig", sb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// 已安装的版本号可能来自不同变体：不得忽略本次下发的签名包而静默复用旧二进制。
func TestInstalledVersionRejectsDifferentPackage(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Versions: filepath.Join(dir, "versions"), Incoming: filepath.Join(dir, "incoming"), Arch: "amd64"}
	for _, d := range []string{p.Versions, p.Incoming} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	ver := "1.30.6-1"
	drop(t, p, priv, ver, makePackage(t, []byte("standard"), nil))
	if err := p.install(ver); err != nil {
		t.Fatal(err)
	}
	if err := p.checkInstalledPackage(ver); err != nil {
		t.Fatalf("same package: %v", err)
	}
	// 同一二进制允许重新打包；不同二进制（包括另一个变体）不得复用。
	drop(t, p, priv, ver, makePackage(t, []byte("standard"), map[string]string{"VERSION": ver}))
	if err := p.checkInstalledPackage(ver); err != nil {
		t.Fatalf("same binary: %v", err)
	}
	drop(t, p, priv, ver, makePackage(t, []byte("gmssl"), nil))
	if err := p.checkInstalledPackage(ver); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("different binary: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	drop(t, p, other, ver, makePackage(t, []byte("standard"), nil))
	if err := p.checkInstalledPackage(ver); !errors.Is(err, pkgsig.ErrSignature) {
		t.Fatalf("bad signature: %v", err)
	}
	base := filepath.Join(p.Incoming, "syedge-"+ver+".tar.gz")
	_ = os.Remove(base)
	_ = os.Remove(base + ".sig")
	if err := p.checkInstalledPackage(ver); err != nil {
		t.Fatalf("local rollback: %v", err)
	}
}

// 首次热升级之前二进制尚未归档，目标版本不能占用它的版本号，否则无法回滚。
func TestOriginalVersionCollisionIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Bin: filepath.Join(dir, "syedge"), Versions: filepath.Join(dir, "versions")}
	if err := os.WriteFile(p.Bin, []byte("#!/bin/sh\necho 'SyEdge version: SyEdge/1.30.6' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.checkOriginalVersion("1.30.6"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("same version: %v", err)
	}
	if err := p.checkOriginalVersion("1.30.6-1"); err != nil {
		t.Fatalf("distinct version: %v", err)
	}
}

// 正式发布时将真实国密产物签名并交给 wrapper 解包，避免把构建用目录树当成升级包发布。
func TestReleaseGMUpgradePackage(t *testing.T) {
	path := os.Getenv("SYEDGE_GM_UPGRADE_PACKAGE")
	if path == "" {
		t.Skip("release GM upgrade package not provided")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := Paths{Versions: filepath.Join(dir, "versions"), Incoming: filepath.Join(dir, "incoming"), Arch: runtime.GOARCH}
	for _, d := range []string{p.Versions, p.Incoming} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	// 发布包的 VERSION 是升级版本，运行二进制仍报告上游版本。
	tmp := filepath.Join(dir, "unpacked")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unpackPackage(data, tmp); err != nil {
		t.Fatalf("invalid release package: %v", err)
	}
	vb, err := os.ReadFile(filepath.Join(tmp, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	ver := strings.TrimSpace(string(vb))
	if !pkgsig.VersionRe.MatchString(ver) || !strings.HasSuffix(ver, "-1") {
		t.Fatalf("invalid GM version %q", ver)
	}
	sig, err := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge", ver, p.Arch, "gmssl", data)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(sig)
	base := filepath.Join(p.Incoming, "syedge-"+ver+".tar.gz")
	if err := os.WriteFile(base, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".sig", sb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.install(ver); err != nil {
		t.Fatalf("wrapper rejected signed release package: %v", err)
	}
	bin := filepath.Join(p.Versions, ver, binEntry)
	if out, err := exec.Command(bin, "-V").CombinedOutput(); err != nil || !bytes.Contains(out, []byte("enable-ntls")) {
		t.Fatalf("release binary is not GM: %v %s", err, out)
	}
}

// 用真实 syedge 演练热升级：升级、提交、回退、中止，以及坏包被拒绝。需要 root 与已安装的 syedge。
func TestHotUpgradeWithRealSyedge(t *testing.T) {
	src := os.Getenv("SYEDGE_BIN")
	if src == "" {
		src = DataplaneBin()
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		t.Skip("syedge binary not installed")
	}
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("hot upgrade test needs root on linux")
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(filepath.Dir(dir), 0o755) // worker 以 nobody 运行，需要能进入目录读取页面
	for _, d := range []string{"sbin", "versions", "incoming", "run", "logs", "conf", "html"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "html/index.html"), []byte("ok\n"), 0o644)
	port := freePort(t)
	conf := filepath.Join(dir, "conf/syedge.conf")
	_ = os.WriteFile(conf, []byte(fmt.Sprintf(`user nobody nogroup;
worker_processes 2;
pid %[1]s/run/syedge.pid;
error_log %[1]s/logs/error.log;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/run/body;
    proxy_temp_path %[1]s/run/proxy;
    fastcgi_temp_path %[1]s/run/fcgi;
    uwsgi_temp_path %[1]s/run/uwsgi;
    scgi_temp_path %[1]s/run/scgi;
    server { listen 127.0.0.1:%[2]d; location / { root %[1]s/html; } }
}
`, dir, port)), 0o644)
	p := Paths{Bin: filepath.Join(dir, "sbin/syedge"), Versions: filepath.Join(dir, "versions"), Incoming: filepath.Join(dir, "incoming"),
		PidFile: filepath.Join(dir, "run/syedge.pid"), Conf: conf, Arch: runtime.GOARCH, Timeout: 10 * time.Second}
	if err := os.WriteFile(p.Bin, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}

	// 启动 master（与 systemd 相同：通过 Bin 路径启动，热升级时 master 重新执行同一路径）
	if out, err := exec.Command(p.Bin, "-p", dir, "-c", conf).CombinedOutput(); err != nil {
		t.Fatalf("start syedge: %v %s", err, out)
	}
	t.Cleanup(func() {
		for _, f := range []string{p.PidFile + ".oldbin", p.PidFile} {
			if pid, err := readPid(f); err == nil {
				_ = exec.Command("kill", "-TERM", fmt.Sprint(pid)).Run()
			}
		}
		time.Sleep(300 * time.Millisecond)
	})
	get := func() error {
		c := http.Client{Timeout: 2 * time.Second}
		r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return err
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 || string(b) != "ok\n" {
			return fmt.Errorf("status %d body %q", r.StatusCode, b)
		}
		return nil
	}
	if !waitFor(5*time.Second, func() bool { return get() == nil }) {
		lg, _ := os.ReadFile(filepath.Join(dir, "logs/error.log"))
		t.Fatalf("syedge did not start serving: %v\n%s", get(), lg)
	}
	pid0, _ := readPid(p.PidFile)

	// 签名错误的包：拒绝，线上不变
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	drop(t, p, otherPriv, "9.0.0", makePackage(t, bin, nil))
	if err := UpgradeStart(p, "9.0.0"); !errors.Is(err, pkgsig.ErrSignature) {
		t.Fatalf("bad signature: %v", err)
	}
	// 包内含不允许的文件：拒绝
	drop(t, p, priv, "9.0.1", makePackage(t, bin, map[string]string{"etc/passwd": "x"}))
	if err := UpgradeStart(p, "9.0.1"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("bad entry: %v", err)
	}
	// 新二进制 -t 失败：拒绝（用一个只会退出 1 的脚本冒充二进制）
	drop(t, p, priv, "9.0.2", makePackage(t, []byte("#!/bin/sh\nexit 1\n"), nil))
	if err := UpgradeStart(p, "9.0.2"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("failing -t: %v", err)
	}
	if st, _ := os.Lstat(p.Bin); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("binary must not be switched by rejected upgrades")
	}
	if pid, _ := readPid(p.PidFile); pid != pid0 || get() != nil {
		t.Fatal("rejected upgrades must not touch the running master")
	}

	// 正常升级：新旧 master 共存，旧 worker 退出，请求不中断
	drop(t, p, priv, "9.1.0", makePackage(t, bin, map[string]string{"VERSION": "9.1.0"}))
	if err := UpgradeStart(p, "9.1.0"); err != nil {
		t.Fatal(err)
	}
	pid1, _ := readPid(p.PidFile)
	old, err := readPid(p.PidFile + ".oldbin")
	if err != nil || old != pid0 || pid1 == pid0 || !isMaster(pid1) {
		t.Fatalf("after start: new=%d old=%d (%v)", pid1, old, err)
	}
	if err := get(); err != nil {
		t.Fatalf("serving after start: %v", err)
	}
	if link, _ := os.Readlink(p.Bin); link != filepath.Join(p.Versions, "9.1.0/sbin/syedge") {
		t.Fatalf("link = %q", link)
	}
	// 进行中再次升级被拒绝
	if err := UpgradeStart(p, "9.1.0"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("concurrent upgrade: %v", err)
	}
	if err := UpgradeCommit(p); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return !alive(pid0) }) {
		t.Fatal("old master still alive after commit")
	}
	if _, err := os.Stat(p.PidFile + ".oldbin"); err == nil {
		t.Fatal("oldbin pid file left behind")
	}
	if err := get(); err != nil {
		t.Fatal(err)
	}

	// 回退到升级前的版本（首次升级时已把原二进制收进 versions/<版本号>，无需签名包）
	baseVer := ""
	ents, _ := os.ReadDir(p.Versions)
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "9.") && !strings.HasPrefix(e.Name(), ".") {
			baseVer = e.Name()
		}
	}
	if baseVer == "" {
		t.Fatal("original binary was not preserved under versions/")
	}
	if err := UpgradeStart(p, baseVer); err != nil {
		t.Fatalf("rollback start: %v", err)
	}
	pid2, _ := readPid(p.PidFile)
	// 中止：旧 master（9.1.0）重新接管，软链切回
	if err := UpgradeAbort(p); err != nil {
		t.Fatal(err)
	}
	if !waitFor(3*time.Second, func() bool { return workers(pid1) > 0 }) {
		t.Fatal("previous master has no workers after abort")
	}
	if pid, _ := readPid(p.PidFile); pid != pid1 || alive(pid2) && isMaster(pid2) {
		t.Fatalf("after abort pid=%d want %d (new master %d alive=%v)", pid, pid1, pid2, alive(pid2))
	}
	if link, _ := os.Readlink(p.Bin); link != filepath.Join(p.Versions, "9.1.0/sbin/syedge") {
		t.Fatalf("link after abort = %q", link)
	}
	if !waitFor(5*time.Second, func() bool { return get() == nil }) {
		lg, _ := os.ReadFile(filepath.Join(dir, "logs/error.log"))
		ps, _ := exec.Command("ps", "-eo", "pid,ppid,args").Output()
		t.Fatalf("not serving after abort: %v\n%s\n%s", get(), lg, ps)
	}
	if err := UpgradeCommit(p); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("commit without upgrade: %v", err)
	}
}
