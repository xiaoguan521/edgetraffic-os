package render

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
)

// 访问规则（P10 规则积木，规格书 12.5）：界面上用“条件 + 动作”搭规则，由渲染器生成配置。
//
// 原生规则只用官方模块（geo / map / if+return / limit_req / auth_request），所有版本都能用；
// JWT 校验需要 Lua 附加包（syedge-lua），见 lua.go。用户输入只经下面的校验与转义函数进入配置，
// 不拼接任何未校验的字符串。
//
// 执行顺序按请求处理阶段：拒绝 / 跳转 → JWT（rewrite，Lua）→ 限速（preaccess）→ 外部鉴权（access）。
// 同一阶段内按规则顺序。ACME 验证路径不受任何规则影响。

// 规则数量上限。
const (
	MaxRules       = 32
	MaxConditions  = 8
	MaxRuleCIDRs   = 256
	MaxIPSets      = 64
	MaxIPSetCIDRs  = 20000
	maxMatchValue  = 256
	authLocPrefix  = "/__etos_auth_"
	reservedPrefix = "/__etos"
)

// Rule 是站点的一条访问规则：全部条件同时满足时执行动作；没有条件表示对所有请求生效。
type Rule struct {
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions"`
	Action     RuleAction  `json:"action"`
}

// Condition 是规则的一个条件。
type Condition struct {
	Kind    string   `json:"kind"`             // ip | ip_set | header | user_agent | path | method
	Negate  bool     `json:"negate,omitempty"` // 取反（不满足时为真）
	CIDRs   []string `json:"cidrs,omitempty"`  // ip
	Set     string   `json:"set,omitempty"`    // ip_set：节点组的 IP 集（地域库）名称
	Header  string   `json:"header,omitempty"` // header
	Op      string   `json:"op,omitempty"`     // header / user_agent：equals | prefix | contains | regex | exists；path：prefix | exact | regex
	Value   string   `json:"value,omitempty"`
	Methods []string `json:"methods,omitempty"` // method
}

// RuleAction 是规则命中后的动作。
type RuleAction struct {
	Type string `json:"type"` // deny | redirect | rate_limit | auth_request | jwt

	Status       int    `json:"status,omitempty"`        // deny：401 403 404 429 444 451 503；redirect：301 302 307 308
	To           string `json:"to,omitempty"`            // redirect：http(s) 绝对地址
	PreservePath bool   `json:"preserve_path,omitempty"` // redirect：在地址后追加原请求的路径与参数

	Rate      int    `json:"rate,omitempty"`       // rate_limit：每 per 允许的请求数
	Per       string `json:"per,omitempty"`        // s | m
	Burst     int    `json:"burst,omitempty"`      // 允许的突发
	Key       string `json:"key,omitempty"`        // ip（默认）| header
	KeyHeader string `json:"key_header,omitempty"` // key=header 时按该请求头计数

	Auth *AuthAction `json:"auth,omitempty"` // auth_request
	JWT  *JWTAction  `json:"jwt,omitempty"`  // jwt（需要 Lua 附加包）
}

// AuthAction：外部鉴权。对鉴权服务发子请求（不带请求体），2xx 放行，401 / 403 原样返回给客户端。
type AuthAction struct {
	Upstream string `json:"upstream"` // 节点组的上游服务名（地址受回源网段约束）
	Path     string `json:"path"`     // 鉴权接口路径，例如 /auth/verify
}

// IPSet 是节点组的命名 IP 集（例如按国家 / 地区从 RIR 分配文件导入的地域库）。
type IPSet struct {
	Name  string   `json:"name"`
	CIDRs []string `json:"cidrs"`
}

var (
	ErrRule       = errors.New("render: invalid rule")
	headerNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	methodSet     = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}
	denyStatus    = map[int]bool{401: true, 403: true, 404: true, 429: true, 444: true, 451: true, 503: true}
	redirectCodes = map[int]bool{301: true, 302: true, 307: true, 308: true}
)

func ruleErr(site string, i int, format string, a ...any) error {
	return fmt.Errorf("%w: site %q rule %d: %s", ErrRule, site, i+1, fmt.Sprintf(format, a...))
}

// printable 报告 s 只含可打印 ASCII（不含控制字符与非 ASCII）。
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// confQuote 把字符串写成配置中的双引号字符串：只转义反斜杠与双引号（调用方已保证可打印 ASCII）。
func confQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// pcreQuote 把字面量转成正则中的字面匹配。
func pcreQuote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(`\.+*?()|[]{}^$/#&-~ `, c) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// headerVar 把请求头名转成变量名（$http_x_api_key）。
func headerVar(h string) string {
	return "$http_" + strings.ToLower(strings.ReplaceAll(h, "-", "_"))
}

func validateCIDRs(cidrs []string, max int) ([]string, error) {
	if len(cidrs) == 0 || len(cidrs) > max {
		return nil, fmt.Errorf("需要 1–%d 个网段", max)
	}
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		var p netip.Prefix
		var err error
		if strings.Contains(c, "/") {
			p, err = netip.ParsePrefix(c)
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(c); err == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%q 不是 IP 或网段", c)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}

func validateRegex(v string) error {
	if len(v) == 0 || len(v) > maxMatchValue || !printable(v) {
		return errors.New("正则表达式为空、过长或含不可打印字符")
	}
	if _, err := syntax.Parse(v, syntax.Perl); err != nil {
		return fmt.Errorf("正则表达式无效：%v", err)
	}
	return nil
}

// validateRules 校验站点的规则（在 Input.Validate 中调用）。
func validateRules(in *Input, s *Site) error {
	if len(s.Rules) > MaxRules {
		return fmt.Errorf("%w: site %q has more than %d rules", ErrRule, s.Name, MaxRules)
	}
	sets := map[string]bool{}
	for _, st := range in.IPSets {
		sets[st.Name] = true
	}
	ups := map[string]bool{}
	for _, u := range in.Upstreams {
		ups[u.Name] = true
	}
	names := map[string]bool{}
	auth := 0
	for i := range s.Rules {
		r := &s.Rules[i]
		if err := ValidateName(r.Name); err != nil {
			return ruleErr(s.Name, i, "名称只能使用小写字母、数字和连字符")
		}
		if names[r.Name] {
			return ruleErr(s.Name, i, "规则名 %s 重复", r.Name)
		}
		names[r.Name] = true
		if len(r.Conditions) > MaxConditions {
			return ruleErr(s.Name, i, "条件不能超过 %d 个", MaxConditions)
		}
		for j := range r.Conditions {
			c := &r.Conditions[j]
			switch c.Kind {
			case "ip":
				cidrs, err := validateCIDRs(c.CIDRs, MaxRuleCIDRs)
				if err != nil {
					return ruleErr(s.Name, i, "IP 条件：%v", err)
				}
				c.CIDRs = cidrs
			case "ip_set":
				if !sets[c.Set] {
					return ruleErr(s.Name, i, "IP 集 %q 不存在", c.Set)
				}
			case "header", "user_agent":
				if c.Kind == "header" && !headerNameRe.MatchString(c.Header) {
					return ruleErr(s.Name, i, "请求头名 %q 无效", c.Header)
				}
				switch c.Op {
				case "exists":
				case "equals", "prefix", "contains":
					if c.Value == "" || len(c.Value) > maxMatchValue || !printable(c.Value) {
						return ruleErr(s.Name, i, "匹配值为空、过长或含不可打印字符")
					}
				case "regex":
					if err := validateRegex(c.Value); err != nil {
						return ruleErr(s.Name, i, "%v", err)
					}
				default:
					return ruleErr(s.Name, i, "未知的匹配方式 %q", c.Op)
				}
			case "path":
				switch c.Op {
				case "prefix", "exact":
					if err := ValidatePath(c.Value); err != nil {
						return ruleErr(s.Name, i, "%v", err)
					}
				case "regex":
					if err := validateRegex(c.Value); err != nil {
						return ruleErr(s.Name, i, "%v", err)
					}
				default:
					return ruleErr(s.Name, i, "路径匹配方式只能是 prefix / exact / regex")
				}
			case "method":
				if len(c.Methods) == 0 {
					return ruleErr(s.Name, i, "至少选择一个请求方法")
				}
				for _, m := range c.Methods {
					if !methodSet[m] {
						return ruleErr(s.Name, i, "请求方法 %q 不支持", m)
					}
				}
			default:
				return ruleErr(s.Name, i, "未知的条件类型 %q", c.Kind)
			}
		}
		a := &r.Action
		switch a.Type {
		case "deny":
			if !denyStatus[a.Status] {
				return ruleErr(s.Name, i, "拒绝的状态码只能是 401 / 403 / 404 / 429 / 444 / 451 / 503")
			}
		case "redirect":
			if !redirectCodes[a.Status] {
				return ruleErr(s.Name, i, "跳转状态码只能是 301 / 302 / 307 / 308")
			}
			u, err := url.Parse(a.To)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(a.To) > 512 || !printable(a.To) ||
				strings.ContainsAny(a.To, "$\" \\{};") {
				return ruleErr(s.Name, i, "跳转地址必须是 http(s) 绝对地址，且不能含 $ \" 空格 \\ { } ;")
			}
		case "rate_limit":
			if a.Rate < 1 || a.Rate > 100000 || (a.Per != "s" && a.Per != "m") || a.Burst < 0 || a.Burst > 100000 {
				return ruleErr(s.Name, i, "限速为每秒 / 每分钟 1–100000 次，突发 0–100000")
			}
			switch a.Key {
			case "", "ip":
			case "header":
				if !headerNameRe.MatchString(a.KeyHeader) {
					return ruleErr(s.Name, i, "计数请求头名 %q 无效", a.KeyHeader)
				}
			default:
				return ruleErr(s.Name, i, "限速计数方式只能是 ip / header")
			}
		case "auth_request":
			if a.Auth == nil || !ups[a.Auth.Upstream] {
				return ruleErr(s.Name, i, "外部鉴权必须选择节点组中已有的上游服务")
			}
			if err := ValidatePath(a.Auth.Path); err != nil {
				return ruleErr(s.Name, i, "鉴权接口路径：%v", err)
			}
			if auth++; auth > 1 {
				return ruleErr(s.Name, i, "每个站点只能有一条外部鉴权规则")
			}
		case "jwt":
			if err := validateJWT(a.JWT); err != nil {
				return ruleErr(s.Name, i, "%v", err)
			}
		default:
			return ruleErr(s.Name, i, "未知的动作 %q", a.Type)
		}
	}
	return nil
}

func validateIPSets(in *Input) error {
	if len(in.IPSets) > MaxIPSets {
		return fmt.Errorf("%w: more than %d IP sets", ErrRule, MaxIPSets)
	}
	seen := map[string]bool{}
	for i := range in.IPSets {
		st := &in.IPSets[i]
		if err := ValidateName(st.Name); err != nil || seen[st.Name] {
			return fmt.Errorf("%w: IP set name %q invalid or duplicated", ErrRule, st.Name)
		}
		seen[st.Name] = true
		cidrs, err := validateCIDRs(st.CIDRs, MaxIPSetCIDRs)
		if err != nil {
			return fmt.Errorf("%w: IP set %s: %v", ErrRule, st.Name, err)
		}
		st.CIDRs = cidrs
	}
	return nil
}

// ---- 生成 ----

// ruleVar 返回规则的命中变量名（不带 $）。
func ruleVar(site string, i int) string {
	return "etos_" + strings.ReplaceAll(site, "-", "_") + "_r" + strconv.Itoa(i)
}

func setVar(name string) string { return "etos_set_" + strings.ReplaceAll(name, "-", "_") }

// matchKey 返回条件在 map 中的正则键（已转成配置字符串）。
func matchKey(op, value string, ci bool) string {
	flag := "~"
	if ci {
		flag = "~*"
	}
	switch op {
	case "equals", "exact":
		return confQuote(flag + "^" + pcreQuote(value) + "$")
	case "prefix":
		return confQuote(flag + "^" + pcreQuote(value))
	case "contains":
		return confQuote(flag + pcreQuote(value))
	default: // regex
		return confQuote(flag + value)
	}
}

// renderRulesHTTP 生成站点规则在 http 层的 geo / map / limit_req_zone（写在站点配置文件开头）。
func renderRulesHTTP(s Site) string {
	if len(s.Rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# 访问规则（渲染器生成，禁止手改）\n")
	one, zero := "1", "0"
	for i, r := range s.Rules {
		v := ruleVar(s.Name, i)
		fmt.Fprintf(&b, "\n# 规则 %s\n", r.Name)
		key := "$etos_not_acme"
		for j, c := range r.Conditions {
			cv := fmt.Sprintf("%s_c%d", v, j)
			hit, miss := one, zero
			if c.Negate {
				hit, miss = zero, one
			}
			switch c.Kind {
			case "ip":
				fmt.Fprintf(&b, "geo  $%s {\n    default  %s;\n", cv, miss)
				for _, p := range c.CIDRs {
					fmt.Fprintf(&b, "    %s  %s;\n", p, hit)
				}
				b.WriteString("}\n")
			case "ip_set":
				fmt.Fprintf(&b, "map  $%s  $%s {\n    1  %s;\n    default  %s;\n}\n", setVar(c.Set), cv, hit, miss)
			case "header", "user_agent":
				src := "$http_user_agent"
				if c.Kind == "header" {
					src = headerVar(c.Header)
				}
				fmt.Fprintf(&b, "map  %s  $%s {\n", src, cv)
				if c.Op == "exists" {
					fmt.Fprintf(&b, "    \"\"  %s;\n    default  %s;\n}\n", miss, hit)
					break
				}
				fmt.Fprintf(&b, "    %s  %s;\n    default  %s;\n}\n", matchKey(c.Op, c.Value, c.Kind == "user_agent" || c.Op != "regex"), hit, miss)
			case "path":
				fmt.Fprintf(&b, "map  $uri  $%s {\n    %s  %s;\n    default  %s;\n}\n", cv, matchKey(c.Op, c.Value, false), hit, miss)
			case "method":
				ms := append([]string(nil), c.Methods...)
				sort.Strings(ms)
				fmt.Fprintf(&b, "map  $request_method  $%s {\n    %s  %s;\n    default  %s;\n}\n", cv, confQuote("~^("+strings.Join(ms, "|")+")$"), hit, miss)
			}
			key += "$" + cv
		}
		fmt.Fprintf(&b, "map  %s  $%s {\n    %s  1;\n    default  0;\n}\n", confQuote(key), v, confQuote(strings.Repeat("1", len(r.Conditions)+1)))
		if r.Action.Type == "rate_limit" {
			src := "$binary_remote_addr"
			if r.Action.Key == "header" {
				src = headerVar(r.Action.KeyHeader)
			}
			fmt.Fprintf(&b, "map  $%s  $%s_key {\n    1  %s;\n    default  \"\";\n}\n", v, v, src)
			fmt.Fprintf(&b, "limit_req_zone  $%s_key  zone=%s:10m  rate=%dr/%s;\n", v, v, r.Action.Rate, r.Action.Per)
		}
	}
	b.WriteString(renderJWTHTTP(s))
	return b.String()
}

// renderRulesServer 生成 server 块内的规则动作。
func renderRulesServer(s Site) string {
	if len(s.Rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n    # 访问规则：拒绝 / 跳转 → JWT → 限速 → 外部鉴权\n")
	for i, r := range s.Rules {
		v := ruleVar(s.Name, i)
		a := r.Action
		switch a.Type {
		case "deny":
			fmt.Fprintf(&b, "    if ($%s) {\n        return  %d;\n    }\n", v, a.Status)
		case "redirect":
			to := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.To)
			if a.PreservePath {
				to = strings.TrimRight(to, "/") + "$request_uri"
			}
			fmt.Fprintf(&b, "    if ($%s) {\n        return  %d  \"%s\";\n    }\n", v, a.Status, to)
		}
	}
	if jwt := renderJWTRewrite(s); jwt != "" {
		b.WriteString(jwt)
	}
	for i, r := range s.Rules {
		if r.Action.Type == "rate_limit" {
			v := ruleVar(s.Name, i)
			if r.Action.Burst > 0 {
				fmt.Fprintf(&b, "    limit_req  zone=%s  burst=%d  nodelay;\n", v, r.Action.Burst)
			} else {
				fmt.Fprintf(&b, "    limit_req  zone=%s;\n", v)
			}
		}
	}
	for i, r := range s.Rules {
		if r.Action.Type == "auth_request" {
			// 规则未命中时，鉴权子请求在本机直接返回 204（不访问鉴权服务）；
			// 主请求先在 rewrite 阶段算出命中结果，子请求共享该变量
			fmt.Fprintf(&b, "    set  $etos_auth_gate  $%s;\n    auth_request  %s%s;\n", ruleVar(s.Name, i), authLocPrefix, strings.ReplaceAll(s.Name, "-", "_"))
		}
	}
	return b.String()
}

// renderAuthLocation 生成外部鉴权的内部 location。
func renderAuthLocation(s Site) string {
	for _, r := range s.Rules {
		if r.Action.Type != "auth_request" || r.Action.Auth == nil {
			continue
		}
		a := r.Action.Auth
		return fmt.Sprintf(`
    location = %s%s {
        internal;
        if ($etos_auth_gate != "1") {
            return  204;
        }
        # location 内出现 proxy_set_header 时须重发基线的全部头（规格书 6.3）
        proxy_set_header  Host               $host;
        proxy_set_header  X-Real-IP          $remote_addr;
        proxy_set_header  X-Forwarded-For    $proxy_add_x_forwarded_for;
        proxy_set_header  X-Forwarded-Proto  $scheme;
        proxy_set_header  X-Request-ID       $request_id;
        proxy_set_header  Connection         "";
        proxy_set_header  X-Original-URI     $request_uri;
        proxy_set_header  X-Original-Method  $request_method;
        proxy_set_header  Content-Length     "";
        proxy_pass_request_body  off;
        proxy_connect_timeout    3s;
        proxy_read_timeout       5s;
        proxy_pass        http://up_%s%s;
    }
`, authLocPrefix, strings.ReplaceAll(s.Name, "-", "_"), a.Upstream, a.Path)
	}
	return ""
}

// renderIPSets 生成节点组 IP 集的 geo 变量（被规则的 ip_set 条件引用）。
func renderIPSets(sets []IPSet) string {
	var b strings.Builder
	b.WriteString("# IP 集 / 地域库（渲染器生成，禁止手改）\n")
	for _, st := range sets {
		fmt.Fprintf(&b, "\ngeo  $%s {\n    default  0;\n", setVar(st.Name))
		for _, p := range st.CIDRs {
			fmt.Fprintf(&b, "    %s  1;\n", p)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// HasRules 报告是否有站点配置了访问规则。
func (in Input) HasRules() bool {
	for _, s := range in.Sites {
		if len(s.Rules) > 0 {
			return true
		}
	}
	return false
}

// NormalizeCIDRs 校验并规范化网段列表（控制台保存 IP 集时使用）。
func NormalizeCIDRs(cidrs []string, max int) ([]string, error) { return validateCIDRs(cidrs, max) }
