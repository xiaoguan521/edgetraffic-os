package render

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"
	"testing"
)

func rsaPub(t *testing.T, bits int) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

const hsSecret = `0123456789abcdef0123456789abcdef"}; os.execute("id") --`

func luaInput(t *testing.T) Input {
	in := sampleInput(t)
	in.Sites[0].Rules = []Rule{
		{Name: "api-hs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/api/"}},
			Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hsSecret, Source: "header", Issuer: `iss"]]..x`, Audience: "shop", Leeway: 30, SubjectHeader: "X-User"}}},
		{Name: "app-rs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/app/"}},
			Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "RS256", PublicKey: rsaPub(t, 2048), Source: "cookie", Cookie: "session_token"}}},
	}
	return in
}

func TestLuaRender(t *testing.T) {
	b, err := Render(luaInput(t))
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syapp.conf"])
	if !strings.HasPrefix(strings.SplitN(main, "\nuser ", 2)[0], "# EdgeTraffic OS") || !strings.Contains(main, "load_module  modules/lua/ngx_http_lua_module.so;") {
		t.Fatalf("load_module missing:\n%s", main[:300])
	}
	if strings.Index(main, "load_module") > strings.Index(main, "events {") {
		t.Fatal("load_module must precede events")
	}
	for _, want := range []string{
		`lua_package_path   "/opt/syapp/modules/lua/lualib/?.lua";`,
		`lua_package_cpath  "/opt/syapp/modules/lua/lualib/?.disabled";`,
		`[\034shop/api-hs\034] = { alg = \034HS256\034, key_file = \034/opt/syapp/conf/releases/r000002/lua/jwt-shop-api-hs.key\034`,
		`key_file = \034/opt/syapp/conf/releases/r000002/lua/jwt-shop-app-rs.pem\034`,
		`cookie = \034session_token\034`,
	} {
		w := strings.ReplaceAll(want, `\034`, `"`)
		if !strings.Contains(main, w) {
			t.Errorf("syapp.conf lacks %q", w)
		}
	}
	// 密钥不进配置文本，只进 0640 的 .key 文件
	if strings.Contains(main, "0123456789abcdef") || strings.Contains(string(b.Files["conf.d/site-shop.conf"]), "0123456789abcdef") {
		t.Fatal("secret leaked into config text")
	}
	if string(b.Files["lua/jwt-shop-api-hs.key"]) != hsSecret || !strings.Contains(string(b.Files["lua/jwt-shop-app-rs.pem"]), "BEGIN PUBLIC KEY") {
		t.Fatal("key files")
	}
	site := string(b.Files["conf.d/site-shop.conf"])
	for _, want := range []string{
		"set  $etos_shop_r0_jwt  \"\";\n    if ($etos_shop_r0) {\n        set_by_lua_block  $etos_shop_r0_jwt  { return require(\"etos.jwt\").verify(\"shop/api-hs\") }\n    }\n    if ($etos_shop_r0_jwt) {\n        return  401;",
		`set_by_lua_block  $etos_shop_r1_jwt  { return require("etos.jwt").verify("shop/app-rs") }`,
		"add_header  WWW-Authenticate  $etos_shop_jwt_www  always;",
		`map  "$etos_shop_r0_jwt$etos_shop_r1_jwt"  $etos_shop_jwt_www {`,
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site conf lacks %q", want)
		}
	}
	// 生成的 Lua 中，每个字符串字面量只含安全字符或 \ddd：不会出现能跳出字面量或块的字符
	lits := regexp.MustCompile(`"((?:[^"\\]|\\[0-9]{3})*)"`).FindAllStringSubmatch(main[strings.Index(main, "init_by_lua_block"):], -1)
	for _, l := range lits {
		if strings.ContainsAny(l[1], `{}$;'[]`) {
			t.Errorf("unsafe Lua literal %q", l[1])
		}
	}
	if !strings.Contains(main, `iss = "iss\034\093\093..x"`) {
		t.Error("issuer not escaped as \\ddd")
	}
	// 只用原生规则时不加载 Lua
	if p, _ := Render(rulesInput(t)); strings.Contains(string(p.Files["syapp.conf"]), "lua_") {
		t.Error("Lua directives without Lua rules")
	}
}

func TestLuaCertOnlyRebase(t *testing.T) {
	cur, err := Render(luaInput(t))
	if err != nil {
		t.Fatal(err)
	}
	next := Repack("r000003", RebaseRevision(cur.Files, "r000002", "r000003"))
	if !strings.Contains(string(next.Files["syapp.conf"]), "/releases/r000003/lua/jwt-shop-api-hs.key") {
		t.Fatal("Lua key paths not rebased")
	}
}

func TestLuaValidation(t *testing.T) {
	bad := map[string]func(*JWTAction){
		"short secret":  func(j *JWTAction) { j.Secret = "short" },
		"alg none":      func(j *JWTAction) { j.Alg = "none" },
		"bad source":    func(j *JWTAction) { j.Source = "query" },
		"bad leeway":    func(j *JWTAction) { j.Leeway = 3600 },
		"bad subj hdr":  func(j *JWTAction) { j.SubjectHeader = "X User" },
		"ctrl in iss":   func(j *JWTAction) { j.Issuer = "a\x00b" },
		"rs256 garbage": func(j *JWTAction) { j.Alg, j.PublicKey = "RS256", "nope" },
	}
	for name, f := range bad {
		in := luaInput(t)
		f(in.Sites[0].Rules[0].Action.JWT)
		if _, err := Render(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in := luaInput(t)
	in.Sites[0].Rules[1].Action.JWT.PublicKey = rsaPub(t, 1024)
	if _, err := Render(in); err == nil {
		t.Error("1024-bit RSA accepted")
	}
	in = luaInput(t)
	in.Sites[0].Rules[1].Action.JWT.Cookie = "a;b"
	if _, err := Render(in); err == nil {
		t.Error("bad cookie name accepted")
	}
}

func TestLuaStr(t *testing.T) {
	if got := luaStr(`a"]]{}$\` + "\n"); got != `"a\034\093\093\123\125\036\092\010"` {
		t.Fatalf("luaStr = %s", got)
	}
}
