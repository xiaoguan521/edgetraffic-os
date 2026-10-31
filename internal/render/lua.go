package render

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Lua 规则（P10）：只在节点装有 syapp-lua 附加包时可用（控制台负责门禁，未装时 syapp -t 失败、agent 自动回滚）。
//
// 不开放客户自定义 Lua：渲染器只生成固定模板，参数一律写成 \ddd 转义的 Lua 字符串字面量（不出现引号、
// 花括号、$ 等任何可改变语法的字符）。密钥写入版本目录的 lua/*.key 文件（0640，预览中不显示），
// 由附加包中审核过的库（/opt/syapp/modules/lua/lualib/etos）在 init 阶段读取，随后关闭文件、
// 进程、代码加载与 C 模块加载能力。

// LuaModulesDir 是附加包模块目录（软链，指向与运行中二进制版本匹配的 lua-<版本>）。
const LuaModulesDir = "/opt/syapp/modules/lua"

// JWTAction：校验 JWT（HS256 / RS256）。失败返回 401，并带 WWW-Authenticate。
type JWTAction struct {
	Alg string `json:"alg"` // HS256 | RS256
	// Secret 是 HS256 密钥：控制台 API 只写不读——保存时加密进 SecretEnc 并清空，渲染前解密填回；永不落库、不回显
	Secret        string `json:"secret,omitempty"`
	SecretEnc     string `json:"secret_enc,omitempty"`     // 控制台加密保存的 HS256 密钥（渲染器忽略）
	HasSecret     bool   `json:"has_secret,omitempty"`     // 仅用于 API 响应：已设置密钥
	PublicKey     string `json:"public_key,omitempty"`     // RS256 公钥（PEM，PUBLIC KEY）
	Source        string `json:"source"`                   // header（Authorization: Bearer）| cookie
	Cookie        string `json:"cookie,omitempty"`         // source=cookie 时的 Cookie 名
	Issuer        string `json:"issuer,omitempty"`         // 要求的 iss（可选）
	Audience      string `json:"audience,omitempty"`       // 要求的 aud（可选）
	Leeway        int    `json:"leeway,omitempty"`         // 时钟偏差容忍秒数（0–300）
	SubjectHeader string `json:"subject_header,omitempty"` // 校验通过后把 sub 写入该请求头转发给上游（可选；客户端自带的同名头总会被清除）
}

var cookieRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func validateJWT(j *JWTAction) error {
	if j == nil {
		return errors.New("缺少 JWT 设置")
	}
	switch j.Alg {
	case "HS256":
		if len(j.Secret) < 32 || len(j.Secret) > 512 || !printable(j.Secret) {
			return errors.New("HS256 密钥需要 32–512 个可打印字符")
		}
	case "RS256":
		blk, rest := pem.Decode([]byte(strings.TrimSpace(j.PublicKey)))
		if blk == nil || blk.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) > 0 {
			return errors.New("RS256 公钥必须是一个 PEM 格式的 PUBLIC KEY")
		}
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		rk, ok := k.(*rsa.PublicKey)
		if err != nil || !ok || rk.N.BitLen() < 2048 {
			return errors.New("RS256 公钥必须是至少 2048 位的 RSA 公钥")
		}
	default:
		return errors.New("算法只能是 HS256 或 RS256")
	}
	switch j.Source {
	case "header":
	case "cookie":
		if !cookieRe.MatchString(j.Cookie) {
			return errors.New("Cookie 名只能使用字母、数字和下划线")
		}
	default:
		return errors.New("令牌来源只能是 header 或 cookie")
	}
	if len(j.Issuer) > 256 || len(j.Audience) > 256 || !printable(j.Issuer) || !printable(j.Audience) {
		return errors.New("iss / aud 过长或含不可打印字符")
	}
	if j.Leeway < 0 || j.Leeway > 300 {
		return errors.New("时钟偏差容忍为 0–300 秒")
	}
	if j.SubjectHeader != "" && !headerNameRe.MatchString(j.SubjectHeader) {
		return errors.New("转发请求头名无效")
	}
	return nil
}

// luaStr 把任意字符串写成 Lua 字面量：除 [A-Za-z0-9_./:-] 外的字节一律写成 \ddd。
func luaStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_./:-", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "\\%03d", c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jwtRule 是一条 JWT 规则在渲染时的信息。
type jwtRule struct {
	ID, Var, File string
	Rule          JWTAction
}

func jwtRules(s Site, rev string) []jwtRule {
	var out []jwtRule
	for i, r := range s.Rules {
		if r.Action.Type != "jwt" || r.Action.JWT == nil {
			continue
		}
		ext := ".key"
		if r.Action.JWT.Alg == "RS256" {
			ext = ".pem"
		}
		out = append(out, jwtRule{ID: s.Name + "/" + r.Name, Var: ruleVar(s.Name, i),
			File: "lua/jwt-" + s.Name + "-" + r.Name + ext, Rule: *r.Action.JWT})
	}
	return out
}

// HasLua 报告版本是否需要 Lua 附加包。
func (in Input) HasLua() bool {
	for _, s := range in.Sites {
		for _, r := range s.Rules {
			if r.Action.Type == "jwt" {
				return true
			}
		}
	}
	return false
}

// luaFiles 返回 Lua 规则需要写入版本目录的密钥文件。
func luaFiles(in Input) map[string][]byte {
	out := map[string][]byte{}
	for _, s := range in.Sites {
		for _, j := range jwtRules(s, in.Revision) {
			if j.Rule.Alg == "HS256" {
				out[j.File] = []byte(j.Rule.Secret)
			} else {
				out[j.File] = []byte(strings.TrimSpace(j.Rule.PublicKey) + "\n")
			}
		}
	}
	return out
}

// renderLuaHTTP 生成 http 层的 Lua 设置与 init 块（写入 syapp.conf）。
func renderLuaHTTP(in Input) string {
	var rules []jwtRule
	for _, s := range in.Sites {
		rules = append(rules, jwtRules(s, in.Revision)...)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	var b strings.Builder
	b.WriteString(`
    # Lua 规则（syapp-lua 附加包）：只加载附加包中审核过的库，不加载任何 C 模块
    lua_package_path   "` + LuaModulesDir + `/lualib/?.lua";
    lua_package_cpath  "` + LuaModulesDir + `/lualib/?.disabled";
    lua_code_cache     on;
    init_by_lua_block {
        require("etos.init").setup({
`)
	for _, j := range rules {
		r := j.Rule
		fmt.Fprintf(&b, "            [%s] = { alg = %s, key_file = %s, source = %s, cookie = %s, iss = %s, aud = %s, leeway = %d, subject_header = %s },\n",
			luaStr(j.ID), luaStr(r.Alg), luaStr(ReleasesDir+in.Revision+"/"+j.File), luaStr(r.Source), luaStr(r.Cookie),
			luaStr(r.Issuer), luaStr(r.Audience), r.Leeway, luaStr(r.SubjectHeader))
	}
	b.WriteString("        })\n    }\n")
	return b.String()
}

// renderJWTRewrite 生成 server 层的 JWT 校验（rewrite 阶段，紧接在拒绝 / 跳转之后）。
//
// 用 set_by_lua 在主 Lua 线程中同步校验（不为每个请求创建协程，开销远低于 access_by_lua），
// 并放在规则自己的 if 里：未命中规则的请求完全不进入 Lua。结果写入每条规则自己的变量，
// 失败时原生 return 401；WWW-Authenticate 由 server 层 add_header 按结果变量输出（值为空时不输出）。
func renderJWTRewrite(s Site) string {
	rules := jwtRules(s, "")
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	for _, j := range rules {
		// 令牌作为参数传入（原生取值），Lua 侧不再查变量；代码只含 \ddd 转义的字面量，放在单引号里也安全
		src := "$http_authorization"
		if j.Rule.Source == "cookie" {
			src = "$cookie_" + j.Rule.Cookie
		}
		fmt.Fprintf(&b, "    set  $%s_jwt  \"\";\n    if ($%s) {\n        set_by_lua  $%s_jwt  'return require(\"etos.jwt\").verify(%s, ngx.arg[1])'  %s;\n    }\n    if ($%s_jwt) {\n        return  401;\n    }\n",
			j.Var, j.Var, j.Var, luaStr(j.ID), src, j.Var)
	}
	fmt.Fprintf(&b, "    add_header  WWW-Authenticate  $%s_jwt_www  always;\n", siteIdent(s.Name))
	return b.String()
}

// renderJWTHTTP 生成 http 层的 WWW-Authenticate 取值（任一 JWT 规则失败时输出）。
func renderJWTHTTP(s Site) string {
	rules := jwtRules(s, "")
	if len(rules) == 0 {
		return ""
	}
	key := ""
	for _, j := range rules {
		key += "$" + j.Var + "_jwt"
	}
	return fmt.Sprintf("map  %s  $%s_jwt_www {\n    \"\"  \"\";\n    default  'Bearer error=\"invalid_token\"';\n}\n", confQuote(key), siteIdent(s.Name))
}

func siteIdent(name string) string { return "etos_" + strings.ReplaceAll(name, "-", "_") }

// luaLoadModules 是 syapp.conf 开头加载附加包模块的指令（相对 /opt/syapp）。
const luaLoadModules = "load_module  modules/lua/ndk_http_module.so;\nload_module  modules/lua/ngx_http_lua_module.so;\n"
