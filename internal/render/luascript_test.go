package render

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateLuaScript(t *testing.T) {
	for _, c := range []struct{ phase, src string }{
		{"init", `ngx.say("x")`},
		{"content", "   \n\t\n"},
		{"content", "\x1bLJ\x02bytecode"},
		{"content", "ngx.say('a')\x00"},
		{"content", "\xff\xfe"},
		{"content", strings.Repeat("-- x\n", 5000)},
	} {
		if _, err := ValidateLuaScript(c.phase, c.src); !errors.Is(err, ErrLuaScript) {
			t.Errorf("%q / %q: want ErrLuaScript, got %v", c.phase, c.src[:min(len(c.src), 20)], err)
		}
	}
	out, err := ValidateLuaScript("content", "\r\nngx.say(\"<h1>Hello</h1>\")   \r\n\r\n")
	if err != nil || out != "ngx.say(\"<h1>Hello</h1>\")\n" {
		t.Fatalf("normalize: %q %v", out, err)
	}
	if err := CheckLuaRoute(Route{Path: "/old", Mode: "redirect"}, "content"); !errors.Is(err, ErrLuaScript) {
		t.Fatal("content script on a redirect route must be rejected")
	}
	if err := CheckLuaRoute(Route{Path: "/old", Mode: "redirect"}, "header_filter"); err != nil {
		t.Fatal(err)
	}
}

// 恶意的脚本文本：试图跳出 *_by_lua_block、注入配置或 init 代码
const evilScript = `ngx.say("}") } location /x { return 200 "pwn"; } --]] ]==] "; os.execute("id") --
ngx.say("<h1>Hello from Lua</h1>")`

func scriptInput(t *testing.T) Input {
	in := sampleInput(t)
	in.Sites[0].Routes = append(in.Sites[0].Routes, Route{Path: "/lua", Match: "exact", Mode: "static", Static: &StaticSpec{IndexHTML: "x"},
		LuaScripts: map[string]string{"content": evilScript, "access": "if ngx.var.arg_deny then return ngx.exit(403) end"}})
	in.Sites[0].Routes[2].LuaScripts = map[string]string{"header_filter": `ngx.header["X-Lua"] = "1"`} // 跳转路由：只能 header_filter
	return in
}

func TestLuaScriptRender(t *testing.T) {
	in := scriptInput(t)
	if !in.HasLua() {
		t.Fatal("scripts must load the Lua modules")
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main, site := string(b.Files["syedge.conf"]), string(b.Files["conf.d/site-shop.conf"])
	if !strings.Contains(main, "load_module  modules/lua/ndk_http_module.so;\nload_module  modules/lua/ngx_http_lua_module.so;") {
		t.Fatal("ndk must be loaded before the lua module")
	}
	dir := routeDir(Route{Path: "/lua", Match: "exact"})
	file := "lua/script-shop-" + dir + "-content.lua"
	if string(b.Files[file]) != strings.TrimSpace(evilScript)+"\n" {
		t.Fatalf("%s = %q", file, b.Files[file])
	}
	// 配置里只有渲染器生成的固定调用；脚本文本只在 lua/ 下的文件中
	for _, want := range []string{
		`content_by_lua_block { return require("etos.script").run("shop:` + dir + `:content") }`,
		`access_by_lua_block { return require("etos.script").run("shop:` + dir + `:access") }`,
		`header_filter_by_lua_block { return require("etos.script").run("shop:` + routeDir(Route{Path: "/old", Match: "exact"}) + `:header_filter") }`,
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site conf lacks %q", want)
		}
	}
	for name, body := range b.Files {
		if strings.HasPrefix(name, "lua/script-") {
			continue
		}
		if strings.Contains(string(body), "pwn") || strings.Contains(string(body), "Hello from Lua") || strings.Contains(string(body), "X-Lua") {
			t.Errorf("script text leaked into %s", name)
		}
	}
	// init：脚本在 etos.init 收紧环境之前加载，路径为本版本目录的绝对路径
	i, j := strings.Index(main, `package.loaded["etos.script"]`), strings.Index(main, `require("etos.init").setup(`)
	if i < 0 || j < 0 || i > j {
		t.Fatal("scripts must be compiled before etos.init tightens the environment")
	}
	if !strings.Contains(main, `"/opt/syedge/conf/releases/r000002/`+file+`"`) || !strings.Contains(main, `load(src, "=" .. id, "t")`) {
		t.Fatal("scripts must be loaded from the version directory as text only")
	}
	for _, blocked := range []string{"socket = true", `k ~= "dump"`, "is not allowed (use local)"} {
		if !strings.Contains(main, blocked) {
			t.Errorf("runtime lacks %q", blocked)
		}
	}
	// 相同输入相同指纹
	b2, _ := Render(in) // 同一输入（证书每次随机生成）
	if b.Manifest.Digest != b2.Manifest.Digest {
		t.Fatal("not deterministic")
	}
}

func TestLuaScriptRejected(t *testing.T) {
	in := scriptInput(t)
	in.Sites[0].Routes[2].LuaScripts = map[string]string{"content": `ngx.say("x")`}
	if _, err := Render(in); !errors.Is(err, ErrLuaScript) {
		t.Fatalf("content script on a redirect route: %v", err)
	}
	in = scriptInput(t)
	in.Sites[0].Routes[1].LuaScripts = map[string]string{"init_worker": `ngx.say("x")`}
	if _, err := Render(in); !errors.Is(err, ErrLuaScript) {
		t.Fatalf("unknown phase: %v", err)
	}
}

// 没有 JWT 规则也没有脚本的版本不加载 Lua 模块（按需加载）。
func TestNoLuaWithoutScripts(t *testing.T) {
	b, err := Render(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b.Files["syedge.conf"]), "load_module") {
		t.Fatal("modules loaded without Lua rules or scripts")
	}
}

// 脚本环境提供的 setmetatable 不能改动已有元表的表：ngx.var / ngx.header / ngx.arg / 共享字典等是整个 worker 共用的表，
// 它们的元表被替换会影响其他站点与规则；只读代理的元表也由此受保护。真实行为由 lua 验收（/lua-meta）验证。
func TestLuaScriptEnvironmentGuardsSetmetatable(t *testing.T) {
	rt := luaScriptRuntime
	if strings.Contains(rt, "setmetatable = setmetatable,") {
		t.Error("the script environment exposes the raw setmetatable")
	}
	for _, want := range []string{"getmetatable(t) ~= nil", "already"} {
		if !strings.Contains(rt, want) {
			t.Errorf("script runtime does not contain the setmetatable guard (%q)", want)
		}
	}
}
