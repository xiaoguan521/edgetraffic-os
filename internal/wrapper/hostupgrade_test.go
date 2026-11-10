package wrapper

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 同机两个实例共用一个数据面二进制（设计 0001 第 5.6 节）：
// 第一个实例升级并提交后软链已是新版本，第二个实例仍能升级自己的 master；第二个实例中止时软链不切回；
// 回退第一个实例再中止时，软链回到它切换之前的版本。需要 root 与真实的 syedge（SYEDGE_BIN）。
func TestHostUpgradeSharedBinaryRealSyedge(t *testing.T) {
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
	_ = os.Chmod(filepath.Dir(dir), 0o755)
	for _, d := range []string{"sbin", "versions", "incoming", "run", "logs", "conf", "html"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "html/index.html"), []byte("ok\n"), 0o644)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	binPath := filepath.Join(dir, "sbin/syedge")
	if err := os.WriteFile(binPath, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	type inst struct {
		p    Paths
		port int
	}
	mk := func(name string) inst {
		port := freePort(t)
		conf := filepath.Join(dir, "conf", name+".conf")
		_ = os.WriteFile(conf, []byte(fmt.Sprintf(`user nobody nogroup;
worker_processes 1;
pid %[1]s/run/%[3]s.pid;
error_log %[1]s/logs/%[3]s.log;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/run/%[3]s-body;
    proxy_temp_path %[1]s/run/%[3]s-proxy;
    fastcgi_temp_path %[1]s/run/%[3]s-fcgi;
    uwsgi_temp_path %[1]s/run/%[3]s-uwsgi;
    scgi_temp_path %[1]s/run/%[3]s-scgi;
    server { listen 127.0.0.1:%[2]d; location / { root %[1]s/html; } }
}
`, dir, port, name)), 0o644)
		instance := name
		if name == "default" {
			instance = ""
		}
		p := Paths{Bin: binPath, Versions: filepath.Join(dir, "versions"), Incoming: filepath.Join(dir, "incoming"),
			PidFile: filepath.Join(dir, "run", name+".pid"), Conf: conf, Prefix: dir, Arch: runtime.GOARCH, Timeout: 10 * time.Second,
			Instance: instance, Keys: map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}}
		if out, err := exec.Command(binPath, "-p", dir, "-c", conf).CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v %s", name, err, out)
		}
		return inst{p, port}
	}
	a, b := mk("default"), mk("blue")
	t.Cleanup(func() {
		for _, in := range []inst{a, b} {
			for _, f := range []string{in.p.PidFile + ".oldbin", in.p.PidFile} {
				if pid, err := readPid(f); err == nil {
					_ = exec.Command("kill", "-TERM", fmt.Sprint(pid)).Run()
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	})
	get := func(in inst) error {
		c := http.Client{Timeout: 2 * time.Second}
		r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", in.port))
		if err != nil {
			return err
		}
		defer func() { _ = r.Body.Close() }()
		body, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 || string(body) != "ok\n" {
			return fmt.Errorf("status %d body %q", r.StatusCode, body)
		}
		return nil
	}
	serving := func(in inst) {
		t.Helper()
		if !waitFor(5*time.Second, func() bool { return get(in) == nil }) {
			t.Fatalf("instance on %d not serving: %v", in.port, get(in))
		}
	}
	serving(a)
	serving(b)
	exe := func(in inst) string {
		pid, _ := readPid(in.p.PidFile)
		return masterExe(pid)
	}
	link := func() string { l, _ := os.Readlink(binPath); return l }
	newBin := filepath.Join(dir, "versions/9.1.0/sbin/syedge")
	drop(t, a.p, priv, "9.1.0", makePackage(t, bin, map[string]string{"VERSION": "9.1.0"}))

	// 第一个实例：切换软链并提交
	if err := UpgradeStart(a.p, "9.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := UpgradeCommit(a.p); err != nil {
		t.Fatal(err)
	}
	if link() != newBin || exe(a) != newBin {
		t.Fatalf("after first instance: link=%q exe=%q", link(), exe(a))
	}
	if exe(b) == newBin {
		t.Fatal("second instance must keep running the old binary until it is upgraded")
	}
	// 同一实例重复升级到正在运行的版本：拒绝
	if err := UpgradeStart(a.p, "9.1.0"); !errors.Is(err, ErrUpgrade) || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("repeat upgrade: %v", err)
	}
	// 第二个实例：软链已是新版本，只升级自己的 master；中止时软链不切回
	bOld, _ := readPid(b.p.PidFile)
	if err := UpgradeStart(b.p, "9.1.0"); err != nil {
		t.Fatalf("second instance start: %v", err)
	}
	if exe(b) != newBin {
		t.Fatalf("second instance new master runs %q", exe(b))
	}
	serving(b)
	if err := UpgradeAbort(b.p); err != nil {
		t.Fatal(err)
	}
	if pid, _ := readPid(b.p.PidFile); pid != bOld {
		t.Fatalf("after abort pid=%d want %d", pid, bOld)
	}
	if link() != newBin {
		t.Fatalf("abort of the second instance switched the shared link back: %q", link())
	}
	serving(a)
	serving(b)
	// 重试第二个实例：成功
	if err := UpgradeStart(b.p, "9.1.0"); err != nil {
		t.Fatalf("retry second instance: %v", err)
	}
	if err := UpgradeCommit(b.p); err != nil {
		t.Fatal(err)
	}
	if exe(b) != newBin {
		t.Fatalf("second instance after commit runs %q", exe(b))
	}
	serving(b)

	// 主机整体回退的第一步：第一个实例回到原版本时切换软链；中止后软链回到 9.1.0
	baseVer := ""
	ents, _ := os.ReadDir(a.p.Versions)
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "9.") && !strings.HasPrefix(e.Name(), ".") {
			baseVer = e.Name()
		}
	}
	if baseVer == "" {
		t.Fatal("original binary was not preserved under versions/")
	}
	if err := UpgradeStart(a.p, baseVer); err != nil {
		t.Fatalf("rollback start: %v", err)
	}
	if link() != filepath.Join(dir, "versions", baseVer, "sbin/syedge") {
		t.Fatalf("rollback did not switch the link: %q", link())
	}
	if err := UpgradeAbort(a.p); err != nil {
		t.Fatal(err)
	}
	if link() != newBin {
		t.Fatalf("abort did not restore the link: %q", link())
	}
	serving(a)
	serving(b)
}
