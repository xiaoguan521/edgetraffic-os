package wrapper

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
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

// 在一次性 Linux 测试机上使用真实标准/国密数据面与 Lua 模块演练同机升级、回退和重试。
func TestGMWithLuaMultiInstanceRollback(t *testing.T) {
	std, gm, addon, upgradeAddon := os.Getenv("SYEDGE_TEST_STANDARD_BIN"), os.Getenv("SYEDGE_GM_UPGRADE_PACKAGE"), os.Getenv("SYEDGE_TEST_LUA_PACKAGE"), os.Getenv("SYEDGE_TEST_LUA_UPGRADE_PACKAGE")
	if std == "" || gm == "" || addon == "" || upgradeAddon == "" || runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires Linux root and real standard, GM and Lua release packages")
	}
	dir := t.TempDir()
	for _, d := range []string{"sbin", "versions", "incoming-default", "incoming-blue", "default", "blue", "lua"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin, err := os.ReadFile(std)
	if err != nil {
		t.Fatal(err)
	}
	sharedBin := filepath.Join(dir, "sbin/syedge")
	if err := os.WriteFile(sharedBin, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-C", filepath.Join(dir, "lua"), "-xzf", addon).CombinedOutput(); err != nil {
		t.Fatalf("extract Lua: %v %s", err, out)
	}
	install := exec.Command("bash", filepath.Join(dir, "lua/install.sh"))
	install.Env = append(os.Environ(), "SYEDGE_ROOT="+dir)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install Lua: %v %s", err, out)
	}
	gmData, err := os.ReadFile(gm)
	if err != nil {
		t.Fatal(err)
	}
	luaData, err := os.ReadFile(upgradeAddon)
	if err != nil {
		t.Fatal(err)
	}
	infoDir := filepath.Join(dir, "gm-info")
	if err := os.Mkdir(infoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unpackPackage(gmData, infoDir); err != nil {
		t.Fatal(err)
	}
	vb, err := os.ReadFile(filepath.Join(infoDir, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	ver := strings.TrimSpace(string(vb))
	if !pkgsig.VersionRe.MatchString(ver) {
		t.Fatalf("invalid upgrade version %q", ver)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	put := func(incoming, name, variant string, data []byte) {
		t.Helper()
		sig, err := pkgsig.Sign(priv, "etos-ci-pkg-test", name, ver, runtime.GOARCH, variant, data)
		if err != nil {
			t.Fatal(err)
		}
		sb, _ := json.Marshal(sig)
		file := filepath.Join(incoming, name+"-"+ver+".tar.gz")
		if err := os.WriteFile(file, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file+".sig", sb, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	makeInstance := func(name string) (Paths, int) {
		t.Helper()
		inst := filepath.Join(dir, name)
		port := freePort(t)
		conf := filepath.Join(inst, "syedge.conf")
		text := fmt.Sprintf(`user root root;
worker_processes 1;
pid %[1]s/syedge.pid;
error_log %[1]s/error.log notice;
load_module modules/lua/ndk_http_module.so;
load_module modules/lua/ngx_http_lua_module.so;
events { worker_connections 32; }
http {
    access_log off;
    client_body_temp_path %[1]s/body;
    proxy_temp_path %[1]s/proxy;
    fastcgi_temp_path %[1]s/fcgi;
    uwsgi_temp_path %[1]s/uwsgi;
    scgi_temp_path %[1]s/scgi;
    lua_package_path "%[2]s/modules/lua/lualib/?.lua";
    lua_package_cpath "%[2]s/modules/lua/lualib/?.disabled";
    init_by_lua_block { require("etos.json"); require("resty.lrucache") }
    server { listen 127.0.0.1:%[3]d; return 200 "ok"; }
}
`, inst, dir, port)
		if err := os.WriteFile(conf, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		p := Paths{Bin: sharedBin, Modules: filepath.Join(dir, "modules"), Versions: filepath.Join(dir, "versions"),
			Incoming: filepath.Join(dir, "incoming-"+name), PidFile: filepath.Join(inst, "syedge.pid"), Conf: conf, Prefix: dir,
			Keys: map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}, Arch: runtime.GOARCH, Timeout: 8 * time.Second}
		if name != "default" {
			p.Instance = name
		}
		put(p.Incoming, "syedge", "gmssl", gmData)
		put(p.Incoming, "syedge-lua", "lua", luaData)
		if out, err := exec.Command(sharedBin, "-p", dir, "-c", conf).CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v %s", name, err, out)
		}
		t.Cleanup(func() {
			for _, f := range []string{p.PidFile + ".oldbin", p.PidFile} {
				if pid, err := readPid(f); err == nil {
					_ = exec.Command("kill", "-TERM", fmt.Sprint(pid)).Run()
					_ = waitFor(3*time.Second, func() bool { return !alive(pid) })
				}
			}
		})
		return p, port
	}
	a, pa := makeInstance("default")
	b, pb := makeInstance("blue")
	serving := func(port int) {
		t.Helper()
		if !waitFor(3*time.Second, func() bool {
			client := http.Client{Timeout: time.Second}
			r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if err != nil {
				return false
			}
			_ = r.Body.Close()
			return r.StatusCode == 200
		}) {
			t.Fatalf("port %d stopped serving", port)
		}
	}
	serving(pa)
	serving(pb)
	if err := UpgradeStart(a, ver); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := UpgradeAbort(a); err != nil {
		t.Fatalf("first abort: %v", err)
	}
	serving(pa)
	serving(pb)
	if err := UpgradeStart(a, ver); err != nil {
		t.Fatalf("first retry: %v", err)
	}
	if err := UpgradeCommit(a); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := UpgradeStart(b, ver); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := UpgradeAbort(b); err != nil {
		t.Fatalf("second abort: %v", err)
	}
	serving(pa)
	serving(pb)
	if link, _ := os.Readlink(a.luaLink()); link != "lua-"+ver {
		t.Fatalf("shared module reverted: %q", link)
	}
	if err := UpgradeStart(b, ver); err != nil {
		t.Fatalf("second retry: %v", err)
	}
	if err := UpgradeCommit(b); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	serving(pa)
	serving(pb)
}
