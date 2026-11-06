package wrapper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// syedge-lua 附加包与热升级（P10）：动态模块与 syedge 二进制版本强绑定。
//
// 附加包装在 Modules/lua-<版本>/，配置通过软链 Modules/lua 加载与运行中二进制匹配的模块。
// 节点装有附加包时（Modules/lua 存在），upgrade-start 必须同时拿到目标版本的附加包（agent 与 syedge 包一起放入
// Incoming，wrapper 以 root 验签），在新二进制 -t 之前把软链切到 lua-<目标版本>；任一步失败都切回。
// upgrade-abort 在让旧 master 重新加载配置（HUP）之前先把软链切回，旧 master 加载的仍是旧版本模块。
// 未装附加包的节点完全不受影响。

var luaEntryRe = regexp.MustCompile(`^addon/((standard|gmssl)/(ndk_http_module|ngx_http_lua_module)\.so|lualib/[A-Za-z0-9_][A-Za-z0-9_/.-]*\.lua|VERSION|NOTICE)$`)

const maxLuaEntries = 4000

func (p Paths) luaLink() string          { return filepath.Join(p.Modules, "lua") }
func (p Paths) luaDir(ver string) string { return filepath.Join(p.Modules, "lua-"+ver) }

// luaInstalled 报告节点是否装有附加包。
func (p Paths) luaInstalled() bool {
	if p.Modules == "" {
		return false
	}
	st, err := os.Lstat(p.luaLink())
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// luaReady 报告 lua-<ver> 已完整安装且模块适配 base（standard | gmssl）。
func (p Paths) luaReady(ver, base string) bool {
	d := p.luaDir(ver)
	b, err := os.ReadFile(filepath.Join(d, "BASE"))
	if err != nil || strings.TrimSpace(string(b)) != base {
		return false
	}
	for _, f := range []string{"ndk_http_module.so", "ngx_http_lua_module.so", "lualib/etos/jwt.lua", "lualib/resty/core.lua"} {
		if st, err := os.Stat(filepath.Join(d, f)); err != nil || !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// swapLua 原子地把 Modules/lua 指向 target（相对路径 lua-<版本>）。
func (p Paths) swapLua(target string) error {
	tmp := p.luaLink() + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, p.luaLink())
}

// binBase 从二进制的 -V 输出判断基础变体（国密版带 enable-ntls）。
func binBase(bin string) string {
	out, _ := exec.Command(bin, "-V").CombinedOutput()
	if bytes.Contains(out, []byte("enable-ntls")) {
		return "gmssl"
	}
	return "standard"
}

// installLua 校验签名并把 Incoming 中的 syedge-lua-<ver> 解到 Modules/lua-<ver>（只取 base 对应的模块）。
func (p Paths) installLua(ver, base string) error {
	path := filepath.Join(p.Incoming, brand.Lua()+"-"+ver+".tar.gz")
	data, err := readNoFollow(path, maxPackage)
	if err != nil {
		return fail("this node has the %s add-on: the add-on package for %s is required (%v)", brand.Lua(), ver, err)
	}
	sb, err := readNoFollow(path+".sig", 64<<10)
	if err != nil {
		return fail("read add-on signature: %v", err)
	}
	sig, err := pkgsig.Parse(sb)
	if err != nil {
		return err
	}
	if sig.Name != brand.Lua() || sig.Version != ver || sig.Arch != p.Arch || sig.Variant != "lua" {
		return fail("add-on signature is for %s %s %s %s, want %s %s %s lua", sig.Name, sig.Version, sig.Arch, sig.Variant, brand.Lua(), ver, p.Arch)
	}
	if err := pkgsig.Verify(sig, data, p.Keys); err != nil {
		return err
	}
	tmp := filepath.Join(p.Modules, ".lua-"+ver+".tmp")
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
		return fail("bad add-on package: %v", err)
	}
	tr := tar.NewReader(zr)
	var version []byte
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || n > maxLuaEntries {
			return fail("bad add-on package: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir || name == "install.sh" || name == "uninstall.sh" {
			continue
		}
		if h.Typeflag != tar.TypeReg || !luaEntryRe.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") || h.Size > maxEntry {
			return fail("add-on entry %q not allowed", h.Name)
		}
		rel := strings.TrimPrefix(name, "addon/")
		if strings.HasSuffix(rel, ".so") {
			if !strings.HasPrefix(rel, base+"/") {
				continue // 另一种基础变体的模块
			}
			rel = strings.TrimPrefix(rel, base+"/")
		}
		dst := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		_, err = io.Copy(io.MultiWriter(f, &buf), io.LimitReader(tr, maxEntry))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if rel == "VERSION" {
			version = buf.Bytes()
		}
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(version, &v) != nil || v.Version != ver {
		return fail("add-on VERSION does not match %s", ver)
	}
	if err := os.WriteFile(filepath.Join(tmp, "BASE"), []byte(base+"\n"), 0o644); err != nil {
		return err
	}
	dir := p.luaDir(ver)
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	ok = true
	if !p.luaReady(ver, base) {
		return fail("add-on package for %s is incomplete for the %s base", ver, base)
	}
	return nil
}

// prepareLua 在升级前把附加包切到目标版本，返回原来的软链目标（未装附加包时为空）。
func (p Paths) prepareLua(ver, newBin string) (string, error) {
	if !p.luaInstalled() {
		return "", nil
	}
	prev, err := os.Readlink(p.luaLink())
	if err != nil {
		return "", err
	}
	if prev == "lua-"+ver {
		return "", nil // 同机其他实例已切到目标版本：本实例无需切换，也不切回
	}
	base := binBase(newBin)
	if !p.luaReady(ver, base) {
		if err := p.installLua(ver, base); err != nil {
			return "", err
		}
	}
	if err := p.swapLua("lua-" + ver); err != nil {
		return "", err
	}
	return prev, nil
}
