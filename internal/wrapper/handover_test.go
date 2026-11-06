package wrapper

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// 测试二进制兼作 launcher：Handover 把 sbin/<品牌> 指向它，旧 master 以该路径执行时进入 Launch。
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == brand.ID() {
		Launch(os.Args[0])
	}
	os.Exit(m.Run())
}

func chownR(t *testing.T, dir string, uid, gid int) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			err = os.Lchown(p, uid, gid)
		}
		return err
	})
}

// D5：以 root 运行的 master（升级前启动）在持续请求下无中断地切换为非 root；新 master 只有 CAP_NET_BIND_SERVICE，
// 之后仍可绑定特权端口（reload 新增一个特权端口的 listen）。需要 root、真实的数据面（SYAPP_BIN）与 syapp 用户。
func TestHandoverRealSyapp(t *testing.T) {
	src := os.Getenv("SYAPP_BIN")
	if src == "" {
		src = DataplaneBin
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		t.Skip("syapp binary not installed")
	}
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("needs root on linux")
	}
	u, err := user.Lookup(brand.ID())
	if err != nil {
		t.Skip("no " + brand.ID() + " user")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(filepath.Dir(dir), 0o755)
	for _, d := range []string{"sbin", "versions", "run", "logs", "conf", "html", "cache"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "html/index.html"), []byte("ok\n"), 0o644)
	port := freePort(t)
	priv := 0
	for _, p := range []int{881, 882, 883, 884} { // 一个空闲的特权端口，reload 时新增
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			_ = l.Close()
			priv = p
			break
		}
	}
	conf := filepath.Join(dir, "conf/syapp.conf")
	writeConf := func(extra string) {
		_ = os.WriteFile(conf, []byte(fmt.Sprintf(`worker_processes 2;
pid %[1]s/run/syapp.pid;
error_log %[1]s/logs/error.log;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/cache/body;
    proxy_temp_path %[1]s/cache/proxy;
    fastcgi_temp_path %[1]s/cache/fcgi;
    uwsgi_temp_path %[1]s/cache/uwsgi;
    scgi_temp_path %[1]s/cache/scgi;
    server { listen 127.0.0.1:%[2]d; %[3]s location / { root %[1]s/html; } }
}
`, dir, port, extra)), 0o644)
	}
	writeConf("")
	p := Paths{Bin: filepath.Join(dir, "sbin/syapp"), Versions: filepath.Join(dir, "versions"),
		PidFile: filepath.Join(dir, "run/syapp.pid"), Conf: conf, Arch: runtime.GOARCH, Timeout: 10 * time.Second}
	if err := os.WriteFile(p.Bin, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 旧方式：master 以 root 启动
	if out, err := exec.Command(p.Bin, "-p", dir, "-c", conf).CombinedOutput(); err != nil {
		t.Fatalf("start syapp: %v %s", err, out)
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
		c := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
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
		t.Fatalf("not serving: %v", get())
	}
	pid0, _ := readPid(p.PidFile)
	if u0, _ := ProcUID(pid0); u0 != 0 {
		t.Fatalf("test setup: master uid %d", u0)
	}

	// 运行时目录、日志、缓存、版本库与二进制所在目录交给运行用户（<品牌>-instance nonroot 的准备步骤）
	for _, d := range []string{"run", "logs", "cache", "versions", "sbin"} {
		chownR(t, filepath.Join(dir, d), uid, gid)
	}
	_ = os.Lchown(p.Bin, 0, 0) // 二进制文件本身仍由 root 拥有

	var failures, total atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			total.Add(1)
			if get() != nil {
				failures.Add(1)
			}
		}
	}()
	err = Handover(p, self, uid, gid)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	<-done
	if err != nil {
		lg, _ := os.ReadFile(filepath.Join(dir, "logs/error.log"))
		t.Fatalf("handover: %v\n%s", err, lg)
	}
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d requests failed during the handover", n, total.Load())
	}
	pid1, _ := readPid(p.PidFile)
	if pid1 == pid0 || alive(pid0) {
		t.Fatalf("old master %d still alive (new %d)", pid0, pid1)
	}
	if u1, _ := ProcUID(pid1); u1 != uid {
		t.Fatalf("new master uid %d, want %d", u1, uid)
	}
	if procCapEff(pid1) != 1<<capNetBindService {
		t.Fatalf("new master capabilities %x, want only CAP_NET_BIND_SERVICE", procCapEff(pid1))
	}
	for w := range workerPids(pid1) {
		if wu, _ := ProcUID(w); wu != uid {
			t.Fatalf("worker %d uid %d", w, wu)
		}
	}
	link, _ := os.Readlink(p.Bin)
	if !strings.HasPrefix(link, p.Versions+"/") || strings.HasSuffix(link, filepath.Base(self)) {
		t.Fatalf("link not restored to the real binary: %q", link)
	}
	if _, err := os.Stat(filepath.Join(p.Versions, handoverFile)); err == nil {
		t.Fatal("handover file left behind")
	}
	t.Logf("handover: %d requests, 0 failed; master %d → %d (uid %d)", total.Load(), pid0, pid1, uid)

	// 已经不是 root：再次切换什么都不做
	if err := Handover(p, self, uid, gid); !errors.Is(err, ErrNotRoot) {
		t.Fatalf("second handover: %v", err)
	}
	// 非 root 的 master 仍能绑定特权端口（ambient CAP_NET_BIND_SERVICE）
	if priv != 0 {
		writeConf(fmt.Sprintf("listen 127.0.0.1:%d;", priv))
		if out, err := exec.Command(p.Bin, "-p", dir, "-c", conf, "-s", "reload").CombinedOutput(); err != nil {
			t.Fatalf("reload: %v %s", err, out)
		}
		ok := waitFor(5*time.Second, func() bool {
			c := http.Client{Timeout: time.Second}
			r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", priv))
			if err == nil {
				_ = r.Body.Close()
			}
			return err == nil
		})
		if !ok {
			lg, _ := os.ReadFile(filepath.Join(dir, "logs/error.log"))
			t.Fatalf("non-root master could not bind privileged port %d:\n%s", priv, lg)
		}
	}
}
