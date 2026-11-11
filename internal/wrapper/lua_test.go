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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

func luaPackage(t *testing.T, ver string, extra map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"install.sh":                            "#!/bin/sh\n",
		"uninstall.sh":                          "#!/bin/sh\n",
		"addon/standard/ndk_http_module.so":     "std-ndk",
		"addon/standard/ngx_http_lua_module.so": "std-lua",
		"addon/gmssl/ndk_http_module.so":        "gm-ndk",
		"addon/gmssl/ngx_http_lua_module.so":    "gm-lua",
		"addon/lualib/resty/core.lua":           "return {}",
		"addon/lualib/etos/jwt.lua":             "return {}",
		"addon/VERSION":                         fmt.Sprintf(`{"name":"syedge-lua","version":%q}`, ver),
		"addon/NOTICE":                          "notice",
	}
	for k, v := range extra {
		files[k] = v
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(b))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func dropLua(t *testing.T, p Paths, priv ed25519.PrivateKey, ver, variant string, pkg []byte) {
	t.Helper()
	sig, err := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge-lua", ver, p.Arch, variant, pkg)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(sig)
	base := filepath.Join(p.Incoming, "syedge-lua-"+ver+".tar.gz")
	_ = os.WriteFile(base, pkg, 0o644)
	_ = os.WriteFile(base+".sig", sb, 0o644)
}

func TestInstallLuaAcceptsPackagedDefaultPrefix(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Modules: filepath.Join(dir, "modules"), Incoming: filepath.Join(dir, "incoming"), Arch: runtime.GOARCH}
	for _, d := range []string{p.Modules, p.Incoming} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	dropLua(t, p, priv, "1.30.6-1", "lua", luaPackage(t, "1.30.6-1", map[string]string{"addon/PREFIX": "/home/atwasoft/syedge\n"}))
	if err := p.installLua("1.30.6-1", "standard"); err != nil {
		t.Fatal(err)
	}
	if !p.luaReady("1.30.6-1", "standard") {
		t.Fatal("add-on was not installed")
	}
}

// 节点装有 syedge-lua 时的热升级：缺附加包时在任何改动前拒绝；附加包坏 / 签名不符被拒绝；
// 同一发行版本的模块已处于使用中时，不能静默把标准版模块当作国密模块复用。
func TestPrepareLuaRejectsVariantCollision(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Modules: filepath.Join(dir, "modules")}
	ver := "1.30.6-1"
	for _, f := range []string{"ndk_http_module.so", "ngx_http_lua_module.so", "lualib/etos/jwt.lua", "lualib/resty/core.lua", "BASE"} {
		path := filepath.Join(p.luaDir(ver), f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		content := []byte("standard\n")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("lua-"+ver, p.luaLink()); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "syedge")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho enable-ntls >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.prepareLua(ver, bin); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("variant collision: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.luaDir(ver), "BASE")); string(b) != "standard\n" {
		t.Fatalf("modules changed: %q", b)
	}
}

// 正常升级把模块软链切到目标版本（只取对应基础变体的模块）；中止时先切回再让旧 master 重载。
func TestHotUpgradeWithLuaAddon(t *testing.T) {
	src := os.Getenv("SYEDGE_BIN")
	if src == "" {
		src = DataplaneBin()
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		t.Skip("syedge binary not installed")
	}
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("needs root on linux")
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(filepath.Dir(dir), 0o755)
	for _, d := range []string{"sbin", "versions", "incoming", "run", "logs", "conf", "modules/lua-1.0.0/lualib/etos", "modules/lua-1.0.0/lualib/resty"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 一个“已安装”的旧版本附加包（配置不加载模块，这里只验证软链切换；真实加载在 CI 的 lua 验收中）
	for _, f := range []string{"ndk_http_module.so", "ngx_http_lua_module.so", "lualib/etos/jwt.lua", "lualib/resty/core.lua"} {
		_ = os.WriteFile(filepath.Join(dir, "modules/lua-1.0.0", f), []byte("old"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "modules/lua-1.0.0/BASE"), []byte("standard\n"), 0o644)
	if err := os.Symlink("lua-1.0.0", filepath.Join(dir, "modules/lua")); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	conf := filepath.Join(dir, "conf/syedge.conf")
	_ = os.WriteFile(conf, []byte(fmt.Sprintf(`worker_processes 1;
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
    server { listen 127.0.0.1:%[2]d; return 200 "ok\n"; }
}
`, dir, port)), 0o644)
	p := Paths{Bin: filepath.Join(dir, "sbin/syedge"), Modules: filepath.Join(dir, "modules"), Versions: filepath.Join(dir, "versions"),
		Incoming: filepath.Join(dir, "incoming"), PidFile: filepath.Join(dir, "run/syedge.pid"), Conf: conf, Arch: runtime.GOARCH, Timeout: 10 * time.Second}
	_ = os.WriteFile(p.Bin, bin, 0o755)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
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
	link := func() string { l, _ := os.Readlink(p.luaLink()); return l }

	// 1. 没有附加包：拒绝，且什么都没改
	drop(t, p, priv, "9.2.0", makePackage(t, bin, nil))
	if err := UpgradeStart(p, "9.2.0"); !errors.Is(err, ErrUpgrade) || !strings.Contains(err.Error(), "add-on") {
		t.Fatalf("missing add-on: %v", err)
	}
	if link() != "lua-1.0.0" {
		t.Fatalf("link changed to %s", link())
	}
	if _, err := os.Stat(p.PidFile + ".oldbin"); err == nil {
		t.Fatal("upgrade started without the add-on")
	}

	// 2. 坏包：路径穿越、签名变体不符
	dropLua(t, p, priv, "9.2.0", "lua", luaPackage(t, "9.2.0", map[string]string{"addon/lualib/../../../x.lua": "x"}))
	if err := UpgradeStart(p, "9.2.0"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("traversal: %v", err)
	}
	// 签名是 syedge 包的（名称不符）：拒绝
	wrong := luaPackage(t, "9.2.0", nil)
	ws, _ := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge", "9.2.0", p.Arch, "standard", wrong)
	wb, _ := json.Marshal(ws)
	_ = os.WriteFile(filepath.Join(p.Incoming, "syedge-lua-9.2.0.tar.gz"), wrong, 0o644)
	_ = os.WriteFile(filepath.Join(p.Incoming, "syedge-lua-9.2.0.tar.gz.sig"), wb, 0o644)
	if err := UpgradeStart(p, "9.2.0"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("wrong package name: %v", err)
	}
	if _, err := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge-lua", "9.2.0", p.Arch, "standard", wrong); err == nil {
		t.Fatal("syedge-lua signed with a non-lua variant")
	}
	dropLua(t, p, priv, "9.2.0", "lua", luaPackage(t, "9.1.9", nil))
	if err := UpgradeStart(p, "9.2.0"); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("VERSION mismatch: %v", err)
	}
	if link() != "lua-1.0.0" {
		t.Fatalf("link changed to %s after rejected packages", link())
	}

	// 3. 正常升级：软链切到 lua-9.2.0，模块取标准版
	dropLua(t, p, priv, "9.2.0", "lua", luaPackage(t, "9.2.0", nil))
	if err := UpgradeStart(p, "9.2.0"); err != nil {
		t.Fatal(err)
	}
	if link() != "lua-9.2.0" {
		t.Fatalf("link = %s", link())
	}
	if b, _ := os.ReadFile(filepath.Join(p.Modules, "lua-9.2.0/ngx_http_lua_module.so")); string(b) != "std-lua" {
		t.Fatalf("installed module %q", b)
	}
	if _, err := os.Stat(filepath.Join(p.Modules, "lua-9.2.0/install.sh")); err == nil {
		t.Fatal("scripts must not be installed")
	}

	// 4. 中止：软链切回旧版本
	if err := UpgradeAbort(p); err != nil {
		t.Fatal(err)
	}
	if link() != "lua-1.0.0" {
		t.Fatalf("abort left link at %s", link())
	}
	st, _ := p.loadState()
	if st.Phase != "aborted" || st.PrevLua != "lua-1.0.0" {
		t.Fatalf("state %+v", st)
	}

	// 5. 再次升级（附加包已安装，不需要重新投放）并提交
	_ = os.Remove(filepath.Join(p.Incoming, "syedge-lua-9.2.0.tar.gz"))
	if err := UpgradeStart(p, "9.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := UpgradeCommit(p); err != nil {
		t.Fatal(err)
	}
	if link() != "lua-9.2.0" {
		t.Fatalf("after commit link = %s", link())
	}
}

// 附加包的 extlib（独立模式与原生兼容配置包可用的第三方纯 Lua 库，放在 modules/lua-<版本>/extlib/）：
// 升级路径必须接受 addon/extlib 下的 .lua 文件，其他类型与形式仍拒绝。
func TestLuaEntryPatternExtlib(t *testing.T) {
	for _, n := range []string{"addon/extlib/resty/redis.lua", "addon/extlib/tablepool.lua", "addon/extlib/resty/limit/req.lua"} {
		if !luaEntryRe.MatchString(n) {
			t.Errorf("%s should be accepted", n)
		}
	}
	for _, n := range []string{"addon/extlib/resty/x.txt", "addon/extlibx/x.lua", "addon/extlib/resty/x.lua.sh"} {
		if luaEntryRe.MatchString(n) {
			t.Errorf("%s should be rejected", n)
		}
	}
}

// D1 第二阶段：附加包可以携带 extlib 下的编译库（*.so，与基础变体无关）和标准版 / 国密版各一份的 lua-upstream 模块。
// 其他路径、目录穿越、带点或横线的目录名与额外后缀一律拒绝。
func TestLuaEntryPatternNativeModules(t *testing.T) {
	for _, n := range []string{
		"addon/standard/ngx_http_lua_upstream_module.so",
		"addon/gmssl/ngx_http_lua_upstream_module.so",
		"addon/extlib/cjson.so",
		"addon/extlib/redis/parser.so",
		"addon/extlib/rds/parser.so",
		"addon/extlib/librestysignal.so",
	} {
		if !luaEntryRe.MatchString(n) {
			t.Errorf("%s should be accepted", n)
		}
	}
	for _, n := range []string{
		"addon/lualib/x.so",
		"addon/extlib/../x.so",
		"addon/extlib/a/../x.so",
		"addon/extlib/a.b/x.so",
		"addon/extlib/x-y.so",
		"addon/extlib//x.so",
		"addon/extlib/x.so.sh",
		"addon/extlibx/x.so",
		"addon/standard/other.so",
		"addon/other/ngx_http_lua_upstream_module.so",
		"addon/standard/ngx_http_lua_upstream_module.so.sh",
		"addon/gmssl/ngx_http_lua_upstream_module.so/x",
	} {
		if luaEntryRe.MatchString(n) {
			t.Errorf("%s should be rejected", n)
		}
	}
}

// 附加包中的编译库随包安装到 modules/lua-<版本>/extlib/；上游模块只取与基础变体匹配的一份（另一份跳过）。
func TestInstallLuaWithNativeModules(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Modules: filepath.Join(dir, "modules"), Incoming: filepath.Join(dir, "incoming"), Arch: runtime.GOARCH}
	for _, d := range []string{p.Modules, p.Incoming} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	ver := "1.30.6-1"
	pkg := luaPackage(t, ver, map[string]string{
		"addon/extlib/cjson.so":                          "cjson-so",
		"addon/extlib/redis/parser.so":                   "redis-so",
		"addon/standard/ngx_http_lua_upstream_module.so": "std-upstream",
		"addon/gmssl/ngx_http_lua_upstream_module.so":    "gm-upstream",
	})
	dropLua(t, p, priv, ver, "lua", pkg)
	if err := p.installLua(ver, "standard"); err != nil {
		t.Fatalf("add-on with native modules rejected: %v", err)
	}
	want := map[string]string{
		"extlib/cjson.so":                 "cjson-so",
		"extlib/redis/parser.so":          "redis-so",
		"ngx_http_lua_upstream_module.so": "std-upstream",
	}
	for f, body := range want {
		b, err := os.ReadFile(filepath.Join(p.luaDir(ver), f))
		if err != nil || string(b) != body {
			t.Errorf("%s = %q, %v; want %q", f, b, err, body)
		}
	}
	// 不带 extlib 的旧包仍可安装（TestInstallLuaAcceptsPackagedDefaultPrefix 覆盖）；带 .. 的路径被拒绝
	ver2 := "1.30.7-1"
	dropLua(t, p, priv, ver2, "lua", luaPackage(t, ver2, map[string]string{"addon/extlib/a/../x.so": "x"}))
	if err := p.installLua(ver2, "standard"); err == nil {
		t.Fatal("add-on entry with .. accepted")
	}
}

func TestInstallLuaWithExtlib(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Modules: filepath.Join(dir, "modules"), Incoming: filepath.Join(dir, "incoming"), Arch: runtime.GOARCH}
	for _, d := range []string{p.Modules, p.Incoming} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.Keys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	ver := "1.30.6-1"
	pkg := luaPackage(t, ver, map[string]string{
		"addon/extlib/resty/redis.lua": "return {}",
		"addon/extlib/resty/rsa.lua":   "return {}",
		"addon/extlib/tablepool.lua":   "return {}",
	})
	dropLua(t, p, priv, ver, "lua", pkg)
	if err := p.installLua(ver, "standard"); err != nil {
		t.Fatalf("add-on with extlib rejected: %v", err)
	}
	for _, f := range []string{"extlib/resty/redis.lua", "extlib/resty/rsa.lua", "extlib/tablepool.lua"} {
		if _, err := os.Stat(filepath.Join(p.luaDir(ver), f)); err != nil {
			t.Fatalf("%s not installed: %v", f, err)
		}
	}
}
