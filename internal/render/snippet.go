package render

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// 自定义片段（规格书 6.4 高级模式 / 12.7 路径 A）。
//
// 片段先由 conf 包解析成语法树，按插入点做指令白名单检查，再由 conf.Print 重新序列化——
// 用户文本从不原样拼进配置。控制台保存时校验一次，渲染时（Render）再校验一次。

// SnippetScope 是片段的插入点。
type SnippetScope string

const (
	ScopeHTTP     SnippetScope = "http"     // 节点组级，渲染为 conf.d/snippet-<name>.conf
	ScopeServer   SnippetScope = "server"   // 站点 server 块末尾
	ScopeLocation SnippetScope = "location" // 站点某条路由的 location 块末尾
	ScopeLua      SnippetScope = "lua"      // 站点某条路由的自定义 Lua 脚本，片段名为执行阶段（luascript.go）
)

// NamedSnippet 是一个 http 层片段。
type NamedSnippet struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// SnippetContext 是校验片段需要的上下文。
type SnippetContext struct {
	Site       string          // server / location 片段所属站点（root / alias 只能指向该站点目录）
	Upstreams  map[string]bool // 已登记的上游组
	AllowedNet []netip.Prefix  // 允许回源网段
	SitesBase  string          // 当前实例的静态站点目录父路径；空值为默认 system 路径
}

// SnippetIssue 是一条不允许的指令。
type SnippetIssue struct {
	File      string `json:"file,omitempty"`
	Line      int    `json:"line"`
	Directive string `json:"directive"`
	Message   string `json:"message"`
}

func (i SnippetIssue) String() string {
	return fmt.Sprintf("第 %d 行 %s：%s", i.Line, i.Directive, i.Message)
}

const (
	maxSnippetBytes      = 16 << 10
	maxSnippetDirectives = 300
	maxSnippetDepth      = 3
)

var (
	ErrSnippet       = errors.New("invalid snippet")
	snippetNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	upstreamTargetRe = regexp.MustCompile(`^up_([a-z0-9][a-z0-9-]{0,62})$`)
	safeURIPathRe    = regexp.MustCompile(`^[A-Za-z0-9/._~\-]*$`)
)

// 基线代理头（规格书 6.3）：片段不能覆盖，嵌套 location 用到 proxy_* 时自动补齐。
var baselineProxyHeaders = [][2]string{
	{"Host", "$host"},
	{"X-Real-IP", "$remote_addr"},
	{"X-Forwarded-For", "$proxy_add_x_forwarded_for"},
	{"X-Forwarded-Proto", "$scheme"},
	{"X-Request-ID", "$request_id"},
	{"Connection", ""},
}

// 安全头由 snippets/security.conf 统一下发，片段不能覆盖。
var protectedHeaders = map[string]bool{
	"server": true, "x-frame-options": true, "x-content-type-options": true, "referrer-policy": true,
	"strict-transport-security": true, "content-security-policy": true,
}

// 明确禁止的指令（给出更具体的原因；不在白名单中的指令同样会被拒绝）。
var forbidden = map[string]string{
	"include":          "不允许引用外部文件",
	"load_module":      "不允许加载模块",
	"ssi":              "不允许 SSI",
	"autoindex":        "不允许目录列表",
	"proxy_cache_path": "不允许在节点上创建缓存目录",
	"proxy_temp_path":  "不允许改变临时目录",
	"proxy_store":      "不允许把响应写入节点文件",
	"listen":           "监听端口由站点配置决定",
	"server_name":      "域名由站点配置决定",
	"ssl_certificate":  "证书由证书库下发",
	"access_log":       "日志由平台统一管理",
	"error_log":        "日志由平台统一管理",
	"server":           "不允许在片段中新增 server",
}

// 各插入点允许的普通指令。
var commonAllowed = []string{
	"rewrite", "return", "try_files", "expires", "etag", "if_modified_since", "charset", "default_type",
	"client_max_body_size", "client_body_timeout", "client_body_buffer_size", "keepalive_timeout", "send_timeout",
	"limit_req", "limit_conn", "limit_rate", "limit_rate_after", "allow", "deny", "error_page",
	"gzip", "gzip_types", "gzip_comp_level", "gzip_min_length", "gzip_vary", "gzip_proxied", "gzip_buffers", "gzip_http_version", "gzip_disable",
	"sub_filter", "sub_filter_once", "sub_filter_types", "sub_filter_last_modified",
	"proxy_connect_timeout", "proxy_read_timeout", "proxy_send_timeout", "proxy_buffering", "proxy_buffer_size", "proxy_buffers",
	"proxy_busy_buffers_size", "proxy_request_buffering", "proxy_http_version", "proxy_redirect", "proxy_intercept_errors",
	"proxy_next_upstream", "proxy_next_upstream_tries", "proxy_next_upstream_timeout", "proxy_hide_header", "proxy_pass_header",
	"proxy_ignore_headers", "proxy_cookie_domain", "proxy_cookie_path", "proxy_ssl_server_name", "proxy_ssl_name", "proxy_set_header",
	"proxy_max_temp_file_size",
}

var httpOnly = []string{"limit_req_zone", "limit_conn_zone", "map", "server_names_hash_bucket_size", "variables_hash_max_size"}

func set(lists ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, l := range lists {
		for _, n := range l {
			m[n] = true
		}
	}
	return m
}

var (
	allowHTTP     = set(httpOnly, commonAllowed)
	allowServer   = set(commonAllowed, []string{"add_header", "location", "if", "set"})
	allowLocation = set(commonAllowed, []string{"if", "set"})
	allowNested   = set(commonAllowed, []string{"if", "set", "proxy_pass", "root", "alias", "index"}) // server 片段里的 location
)

type checker struct {
	ctx    SnippetContext
	issues []SnippetIssue
	count  int
}

func (c *checker) bad(d *conf.Directive, msg string) {
	c.issues = append(c.issues, SnippetIssue{File: d.File, Line: d.Line, Directive: d.Name, Message: msg})
}

// ValidateSnippet 校验片段并返回规范化后的文本（按 Print 格式，无缩进）。
// err 表示文本无法解析或超出限制；issues 列出每条不允许的指令（为空才可使用）。
func ValidateSnippet(scope SnippetScope, src string, ctx SnippetContext) (string, []SnippetIssue, error) {
	if len(src) > maxSnippetBytes {
		return "", nil, fmt.Errorf("%w: larger than %d bytes", ErrSnippet, maxSnippetBytes)
	}
	ds, err := conf.Parse("snippet", []byte(src))
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrSnippet, err)
	}
	out, issues, err := ValidateDirectives(scope, ds, ctx)
	return out, issues, err
}

// ValidateDirectives 校验已解析的指令（保留各自原始的文件与行号，配置导入用），返回规范化文本。
// 会就地补齐嵌套 location 的基线代理头。
func ValidateDirectives(scope SnippetScope, ds []*conf.Directive, ctx SnippetContext) (string, []SnippetIssue, error) {
	var allow map[string]bool
	switch scope {
	case ScopeHTTP:
		allow = allowHTTP
	case ScopeServer:
		allow = allowServer
	case ScopeLocation:
		allow = allowLocation
	default:
		return "", nil, fmt.Errorf("%w: unknown scope %q", ErrSnippet, scope)
	}
	c := &checker{ctx: ctx}
	ds = c.list(ds, scope, allow, 1)
	if c.count > maxSnippetDirectives {
		return "", nil, fmt.Errorf("%w: more than %d directives", ErrSnippet, maxSnippetDirectives)
	}
	if len(c.issues) > 0 {
		return "", c.issues, nil
	}
	return conf.Print(ds, 0), nil, nil
}

func (c *checker) list(ds []*conf.Directive, scope SnippetScope, allow map[string]bool, depth int) []*conf.Directive {
	for _, d := range ds {
		c.count++
		c.one(d, scope, allow, depth)
	}
	return ds
}

func (c *checker) one(d *conf.Directive, scope SnippetScope, allow map[string]bool, depth int) {
	name := strings.ToLower(d.Name)
	if why, ok := forbidden[name]; ok {
		c.bad(d, why)
		return
	}
	switch {
	case strings.Contains(name, "lua"), strings.HasPrefix(name, "perl"), strings.HasPrefix(name, "js_"):
		c.bad(d, "不允许脚本指令")
		return
	case !allow[name]:
		c.bad(d, "该插入点不允许此指令")
		return
	}
	if depth > maxSnippetDepth {
		c.bad(d, "嵌套过深")
		return
	}
	// 块指令只允许 location / if / map
	if d.IsBlock() && name != "location" && name != "if" && name != "map" {
		c.bad(d, "该指令不能带块")
		return
	}
	switch name {
	case "location":
		if !d.IsBlock() || len(d.Args) == 0 || len(d.Args) > 2 {
			c.bad(d, "location 需要路径和块")
			return
		}
		c.list(d.Block, scope, allowNested, depth+1)
		c.ensureBaseline(d)
	case "if":
		if !d.IsBlock() || len(d.Args) == 0 {
			c.bad(d, "if 需要条件和块")
			return
		}
		for _, x := range d.Block {
			c.count++
			if n := strings.ToLower(x.Name); (n != "return" && n != "rewrite") || x.IsBlock() {
				c.bad(d, "if 块内只允许 return / rewrite")
				return
			}
		}
	case "map":
		if !d.IsBlock() || len(d.Args) != 2 {
			c.bad(d, "map 需要源变量、目标变量和块")
			return
		}
		for _, x := range d.Block {
			c.count++
			if x.IsBlock() || len(x.Args) > 1 || strings.EqualFold(x.Name, "include") {
				c.bad(x, "map 块内只允许“键 值”对")
				return
			}
		}
	case "add_header":
		if len(d.Args) < 2 {
			c.bad(d, "add_header 需要名称和值")
		} else if protectedHeaders[strings.ToLower(d.Args[0])] {
			c.bad(d, "不能覆盖安全头 "+d.Args[0])
		}
	case "proxy_set_header":
		if len(d.Args) < 1 {
			c.bad(d, "proxy_set_header 需要名称")
			return
		}
		h := strings.ToLower(d.Args[0])
		if h == "transfer-encoding" {
			c.bad(d, "不能设置 Transfer-Encoding")
			return
		}
		if scope == ScopeLocation {
			for _, b := range baselineProxyHeaders {
				if strings.EqualFold(b[0], h) {
					c.bad(d, "不能覆盖基线代理头 "+b[0])
					return
				}
			}
		}
	case "proxy_pass":
		if len(d.Args) != 1 {
			c.bad(d, "proxy_pass 需要一个目标")
		} else if err := c.proxyTarget(d.Args[0]); err != nil {
			c.bad(d, err.Error())
		}
	case "root", "alias":
		if len(d.Args) != 1 || !c.underSite(d.Args[0]) {
			c.bad(d, "只能指向本站点目录 "+c.sitesBase()+c.ctx.Site+"/")
		}
	case "set":
		if len(d.Args) != 2 || !strings.HasPrefix(d.Args[0], "$") {
			c.bad(d, "set 需要变量和值")
		}
	}
}

// proxyTarget：只允许已登记上游 http://up_<name>[/path]，或允许回源网段内的 http(s)://IP:port[/path]，不允许变量。
func (c *checker) proxyTarget(t string) error {
	if strings.ContainsAny(t, "$") {
		return errors.New("目标不能包含变量")
	}
	u, err := url.Parse(t)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !safeURIPathRe.MatchString(u.Path) {
		return errors.New("目标格式应为 http://up_<上游名>[/路径]")
	}
	if m := upstreamTargetRe.FindStringSubmatch(u.Host); m != nil {
		if !c.ctx.Upstreams[m[1]] {
			return fmt.Errorf("上游 %s 未登记", m[1])
		}
		return nil
	}
	if err := ValidateUpstream(u.Host, c.ctx.AllowedNet); err != nil {
		return errors.New("目标必须是已登记上游或允许回源网段内的地址")
	}
	return nil
}

func (c *checker) underSite(p string) bool {
	base := c.sitesBase() + c.ctx.Site + "/"
	return c.ctx.Site != "" && strings.HasPrefix(p, base) && !strings.Contains(p, "..") && !strings.ContainsAny(p, "$")
}

func (c *checker) sitesBase() string {
	if c.ctx.SitesBase != "" {
		return c.ctx.SitesBase
	}
	return layout.LegacySystemRoot() + "/conf/current/sites/"
}

// ensureBaseline：location 内出现 proxy_pass / proxy_set_header 时，在块开头补齐缺失的基线代理头（规格书 6.3）。
func (c *checker) ensureBaseline(loc *conf.Directive) {
	need := false
	have := map[string]bool{}
	for _, x := range loc.Block {
		n := strings.ToLower(x.Name)
		if n == "proxy_pass" || n == "proxy_set_header" {
			need = true
		}
		if n == "proxy_set_header" && len(x.Args) > 0 {
			have[strings.ToLower(x.Args[0])] = true
		}
	}
	if !need {
		return
	}
	var pre []*conf.Directive
	for _, b := range baselineProxyHeaders {
		if !have[strings.ToLower(b[0])] {
			pre = append(pre, &conf.Directive{Name: "proxy_set_header", Args: []string{b[0], b[1]}, Line: loc.Line})
		}
	}
	loc.Block = append(pre, loc.Block...)
}

// indentSnippet 把规范化文本缩进 n 级（渲染进模板用）。
func indentSnippet(s string, n int) string {
	if s == "" {
		return ""
	}
	pad := strings.Repeat("    ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// validateInputSnippets 在渲染前重新校验 Input 中的全部片段，并替换为规范化文本。
func (in *Input) validateInputSnippets() error {
	ups := map[string]bool{}
	for _, u := range in.Upstreams {
		ups[u.Name] = true
	}
	check := func(scope SnippetScope, site, src string) (string, error) {
		if strings.TrimSpace(src) == "" {
			return "", nil
		}
		out, issues, err := ValidateSnippet(scope, src, SnippetContext{Site: site, Upstreams: ups, AllowedNet: in.AllowedNet, SitesBase: in.P().SitesDir})
		if err != nil {
			return "", err
		}
		if len(issues) > 0 {
			return "", fmt.Errorf("%w: %s", ErrSnippet, issues[0])
		}
		return out, nil
	}
	seen := map[string]bool{}
	for i := range in.HTTPSnippets {
		s := &in.HTTPSnippets[i]
		if !snippetNameRe.MatchString(s.Name) || seen[s.Name] {
			return fmt.Errorf("%w: bad or duplicate http snippet name %q", ErrSnippet, s.Name)
		}
		seen[s.Name] = true
		out, err := check(ScopeHTTP, "", s.Text)
		if err != nil {
			return fmt.Errorf("http snippet %s: %w", s.Name, err)
		}
		s.Text = out
	}
	for i := range in.Sites {
		st := &in.Sites[i]
		out, err := check(ScopeServer, st.Name, st.ServerSnippet)
		if err != nil {
			return fmt.Errorf("site %s server snippet: %w", st.Name, err)
		}
		st.ServerSnippet = out
		for j := range st.Routes {
			r := &st.Routes[j]
			out, err := check(ScopeLocation, st.Name, r.Snippet)
			if err != nil {
				return fmt.Errorf("site %s route %s snippet: %w", st.Name, r.Path, err)
			}
			r.Snippet = out
		}
	}
	return nil
}
