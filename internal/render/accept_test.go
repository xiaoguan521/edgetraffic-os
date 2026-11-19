package render

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// TestAcceptanceConfig 为 CI 的 P10 验收（scripts/ci/lua-acceptance.sh）生成版本目录：用真实渲染器产出配置，
// 写到默认安装根目录的 conf/releases/<ACCEPT_REV> 并通过真实 syedge -t。只在设置 ACCEPT_REV 时运行（需要 root）。
//
//	ACCEPT_LUA=1        包含 JWT（Lua）规则；=0 生成不含 Lua 的版本（卸载附加包前切换用）
//	ACCEPT_HS_SECRET    HS256 密钥；ACCEPT_RS_PUB  RS256 公钥 PEM 文件
//	ACCEPT_GM=<目录>    国密基础包：sign.crt/sign.key/enc.crt/enc.key（站点 api 同时启用国密双证书）
func TestAcceptanceConfig(t *testing.T) {
	rev := os.Getenv("ACCEPT_REV")
	if rev == "" {
		t.Skip("ACCEPT_REV not set")
	}
	lua := os.Getenv("ACCEPT_LUA") == "1"
	proxy := []Route{{Path: "/", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "backend"}}}
	ipDeny := Rule{Name: "ip-deny", Conditions: []Condition{{Kind: "ip", CIDRs: []string{"10.66.0.0/16"}}}, Action: RuleAction{Type: "deny", Status: 403}}
	bots := Rule{Name: "bots", Conditions: []Condition{{Kind: "user_agent", Op: "contains", Value: "BadBot"}}, Action: RuleAction{Type: "deny", Status: 403}}
	api := Site{Name: "api", Domains: []string{"api.test"}, HTTPS: selfSigned(t, "api.test"), Routes: proxy, Rules: []Rule{
		ipDeny, bots,
		{Name: "old", Conditions: []Condition{{Kind: "path", Op: "exact", Value: "/old"}}, Action: RuleAction{Type: "redirect", Status: 302, To: "https://new.test/", PreservePath: true}},
		{Name: "limited", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/limited/"}}, Action: RuleAction{Type: "rate_limit", Rate: 5, Per: "s"}},
		{Name: "auth", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/auth/"}}, Action: RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "authsvc", Path: "/check"}}},
	}}
	bench3 := Site{Name: "bench3", Domains: []string{"bench3.test"}, Routes: proxy, Rules: []Rule{ipDeny, bots}}
	if lua {
		hs := os.Getenv("ACCEPT_HS_SECRET")
		pub, err := os.ReadFile(os.Getenv("ACCEPT_RS_PUB"))
		if err != nil {
			t.Fatal(err)
		}
		api.Rules = append(api.Rules,
			Rule{Name: "hs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/hs/"}},
				Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hs, Source: "header", Issuer: "etos-ci", Audience: "api", SubjectHeader: "X-User"}}},
			Rule{Name: "rs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/rs/"}},
				Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "RS256", PublicKey: string(pub), Source: "header"}}},
		)
		// 压测站点的“三条常用规则”：IP 拒绝、UA 拒绝（都不命中）+ 对所有请求的 JWT 校验
		bench3.Rules = append(bench3.Rules, Rule{Name: "jwt", Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hs, Source: "header"}}})
		// 自定义 Lua 脚本（docs/lua.md 第 5 节）：生成响应、访问控制、改响应头、受限环境、运行时错误
		hello := `ngx.header.content_type = "text/html"
ngx.say("<h1>Hello from Lua</h1><p>这条响应由 Lua 脚本生成。</p>")`
		if os.Getenv("ACCEPT_BAD_LUA") == "1" {
			hello = "ngx.say(\"unterminated)\nlocal = 1\n"
		}
		static := &StaticSpec{IndexHTML: "static"}
		api.Routes = append(append([]Route(nil), api.Routes...),
			Route{Path: "/lua", Match: "exact", Mode: "static", Static: static, LuaScripts: map[string]string{
				"access":  `if ngx.var.arg_deny then return ngx.exit(403) end`,
				"content": hello,
			}},
			Route{Path: "/lua-sb", Match: "exact", Mode: "static", Static: static, LuaScripts: map[string]string{"content": `local r = {}
local function add(v) r[#r + 1] = type(v) end
add(io) add(os.execute) add(load) add(loadstring) add(ngx.socket) add(getmetatable)
add(string.dump) add(debug) add(package) add(jit) add(rawset) add(setfenv)
r[#r + 1] = tostring(pcall(require, "ffi"))
r[#r + 1] = tostring(pcall(require, "resty.rsa"))
r[#r + 1] = tostring(pcall(require, "string.buffer"))
r[#r + 1] = tostring(pcall(function() leak = 1 end))
r[#r + 1] = tostring(pcall(function() string.rep = nil end))
r[#r + 1] = tostring(pcall(function() ngx.say = nil end))
r[#r + 1] = tostring(pcall(function() ngx.req.get_headers = nil end))
r[#r + 1] = tostring(require("etos.json").decode('{"a":7}').a)
ngx.say(table.concat(r, " "))`}},
			Route{Path: "/lua-meta", Match: "exact", Mode: "static", Static: static, LuaScripts: map[string]string{"content": `local r = {}
r[#r + 1] = tostring(pcall(setmetatable, ngx.var, {}))
r[#r + 1] = tostring(pcall(setmetatable, ngx.header, {}))
r[#r + 1] = tostring(pcall(setmetatable, string, {}))
local t = setmetatable({}, { __index = function() return 5 end })
r[#r + 1] = tostring(t.x)
r[#r + 1] = tostring(pcall(setmetatable, t, {}))
r[#r + 1] = type(ngx.var.uri)
ngx.say(table.concat(r, " "))`}},
			Route{Path: "/lua-ext", Match: "exact", Mode: "static", Static: static, LuaScripts: map[string]string{"content": `local ok1, e1 = pcall(require, "resty.redis")
local ok2, e2 = pcall(require, "resty.mysql")
local ok3, e3 = pcall(require, "cjson")
ngx.say(tostring(ok1), " ", tostring(ok2), " ", tostring(string.find(tostring(e1) .. tostring(e2), "not allowed", 1, true) ~= nil), " ", tostring(ok3), " ", tostring(string.find(tostring(e3), "not allowed", 1, true) ~= nil))`}},
			Route{Path: "/lua-err", Match: "exact", Mode: "static", Static: static, LuaScripts: map[string]string{"content": `error("lua-script-boom")`}},
			Route{Path: "/lua-moved", Match: "exact", Mode: "redirect", Redirect: &RedirectSpec{To: "https://new.test/", Code: 302},
				LuaScripts: map[string]string{"header_filter": `ngx.header["X-Lua"] = "1"`}},
		)
	}
	if gm := os.Getenv("ACCEPT_GM"); gm != "" {
		rd := func(n string) string {
			b, err := os.ReadFile(filepath.Join(gm, n))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		api.HTTPS.GM = &GMPair{SignCertPEM: rd("sign.crt"), SignKeyPEM: rd("sign.key"), EncCertPEM: rd("enc.crt"), EncKeyPEM: rd("enc.key")}
	}
	in := Input{
		Revision:   rev,
		Profile:    Profile{Mode: "system", Root: layout.DefaultSystemRoot()},
		AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RealIPFrom: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		MimeTypes:  DefaultMimeTypes(),
		Upstreams: []UpstreamGroup{
			{Name: "backend", Servers: []UpstreamServer{{Addr: "10.99.0.1:18080"}}},
			{Name: "authsvc", Servers: []UpstreamServer{{Addr: "10.99.0.1:18081"}}},
		},
		Sites: []Site{api, {Name: "bench0", Domains: []string{"bench0.test"}, Routes: proxy}, bench3},
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(in.Profile.ReleasesDir(), rev)
	_ = os.RemoveAll(dir)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	bin := os.Getenv("SYEDGE_BIN")
	if bin == "" {
		bin = filepath.Join(layout.DefaultSystemRoot(), "sbin", "syedge")
	}
	// ACCEPT_BAD_LUA=1：脚本有语法错误。syedge -t 不执行 init 阶段，照常通过；错误在 reload 时出现（lua-acceptance.sh 验证）
	out, err := exec.Command(bin, "-p", in.Profile.lay().RuntimeRoot(), "-e", in.Profile.lay().ErrorLog(), "-t", "-q", "-c", filepath.Join(dir, "syedge.conf")).CombinedOutput()
	if err != nil {
		t.Fatalf("syedge -t failed: %v\n%s", err, out)
	}
}
