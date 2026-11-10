// Package importer 把客户已有的配置（-T 输出或配置目录 tar.gz）转换成站点模型（规格书 12.4）。
//
// 原则：转不了的部分逐行列入报告，一行都不静默丢弃；白名单内但模型表达不了的指令转为自定义片段（需审批）；
// 白名单外的标为“需人工处理”，对应站点导入后保持停用，处理完才能发布。本包只做转换，不写数据库。
package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// 站点转换结果。
const (
	StatusFull    = "full"    // 完全转换为可视化模型
	StatusPartial = "partial" // 部分转换：有片段或需人工处理的指令
	StatusFailed  = "failed"  // 无法转换：站点无法创建
)

// Options 是转换选项。
type Options struct {
	AllowedNet []netip.Prefix // 节点组允许回源网段（直连地址必须在其中）
}

// Issue 是报告中的一条：未转换的指令及原因。Blocking 表示该站点在处理前不能发布。
type Issue struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Directive string `json:"directive"`
	Reason    string `json:"reason"`
	Blocking  bool   `json:"blocking"`
	Location  string `json:"location,omitempty"` // 原配置中的 location 上下文，不表示一定能转换为路由
}

// UpstreamResult 是一个转换成功的上游组。
type UpstreamResult struct {
	Spec render.UpstreamGroup `json:"spec"`
	File string               `json:"file"`
	Line int                  `json:"line"`
	Auto bool                 `json:"auto"` // 由直连地址的 proxy_pass 自动生成
}

// SiteResult 是一个站点（可能由多个 server 块合并而来，例如 80 跳转 + 443）。
type SiteResult struct {
	Name             string            `json:"name"`
	File             string            `json:"file"`
	Line             int               `json:"line"`
	Status           string            `json:"status"`
	Spec             render.Site       `json:"spec"`
	ServerSnippet    string            `json:"server_snippet"`
	LocationSnippets map[string]string `json:"location_snippets"`
	CertFile         string            `json:"cert_file"`
	KeyFile          string            `json:"key_file"`
	CertFound        bool              `json:"cert_found"`
	CertName         string            `json:"cert_name"`
	Issues           []Issue           `json:"issues"`
}

// Blocking 返回站点是否有需人工处理的问题。
func (s *SiteResult) Blocking() bool {
	for _, i := range s.Issues {
		if i.Blocking {
			return true
		}
	}
	return false
}

// CertMaterial 是从压缩包中找到的证书与私钥（不进入报告 JSON）。
type CertMaterial struct {
	CertPEM string
	KeyPEM  string
}

// Summary 是报告摘要。
type Summary struct {
	Servers int `json:"servers"` // http 中的 server 块数
	Sites   int `json:"sites"`
	Full    int `json:"full"`
	Partial int `json:"partial"`
	Failed  int `json:"failed"`
	Issues  int `json:"issues"`
}

// Report 是导入报告。
type Report struct {
	Source       string                  `json:"source"`
	Summary      Summary                 `json:"summary"`
	Sites        []SiteResult            `json:"sites"`
	Upstreams    []UpstreamResult        `json:"upstreams"`
	HTTPSnippet  string                  `json:"http_snippet"` // http 层白名单内的指令
	Global       []Issue                 `json:"global"`       // 全局、stream 等不属于任何站点的内容
	Certificates map[string]CertMaterial `json:"-"`
}

const (
	maxArchive      = 20 << 20
	maxArchiveFiles = 2000
	maxFileSize     = 2 << 20
)

// FromDump 转换 -T 的输出。
func FromDump(b []byte, o Options) (*Report, error) {
	if len(b) > maxArchive {
		return nil, fmt.Errorf("input larger than %d bytes", maxArchive)
	}
	files, err := conf.SplitDump(b)
	if err != nil {
		return nil, err
	}
	if len(files) > maxArchiveFiles {
		return nil, fmt.Errorf("input has more than %d files", maxArchiveFiles)
	}
	return convert("dump", files, nil, o)
}

// FromFiles imports separately uploaded files without accessing the server's filesystem.
func FromFiles(files []conf.File, o Options) (*Report, error) {
	if len(files) == 0 || len(files) > maxArchiveFiles {
		return nil, fmt.Errorf("请选择 1 到 %d 个文件", maxArchiveFiles)
	}
	all := map[string][]byte{}
	var configs []conf.File
	total := 0
	for _, f := range files {
		if f.Name == "" || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, `\`) || strings.Contains(f.Name, "..") || len(f.Body) > maxFileSize {
			return nil, fmt.Errorf("文件 %q 的路径或大小不合法", f.Name)
		}
		key := path.Clean("/" + f.Name)
		if _, exists := all[key]; exists {
			return nil, fmt.Errorf("文件名 %q 重复，请使用压缩包保留目录结构", f.Name)
		}
		total += len(f.Body)
		if total > maxArchive {
			return nil, fmt.Errorf("文件总大小超过 %d bytes", maxArchive)
		}
		all[key] = f.Body
		if strings.HasSuffix(strings.ToLower(key), ".conf") {
			configs = append(configs, conf.File{Name: key, Body: f.Body})
		}
	}
	if len(configs) == 0 {
		return nil, errors.New("请选择主配置 .conf 文件")
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	return convert("files", configs, all, o)
}

// FromArchive 转换配置目录的 tar.gz（可包含证书与私钥）。
func FromArchive(b []byte, o Options) (*Report, error) {
	if len(b) > maxArchive {
		return nil, fmt.Errorf("archive larger than %d bytes", maxArchive)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not a tar.gz archive: %w", err)
	}
	tr := tar.NewReader(io.LimitReader(zr, 4*maxArchive))
	all := map[string][]byte{}
	var files []conf.File
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue // 目录、链接等忽略（不跟随链接）
		}
		if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, `\`) || strings.Contains(h.Name, "..") || h.Size > maxFileSize {
			return nil, fmt.Errorf("archive entry %q rejected (path or size)", h.Name)
		}
		name := path.Clean("/" + h.Name)
		if len(all) >= maxArchiveFiles {
			return nil, fmt.Errorf("archive has more than %d files", maxArchiveFiles)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFileSize))
		if err != nil {
			return nil, err
		}
		all[name] = data
		if strings.HasSuffix(name, ".conf") {
			files = append(files, conf.File{Name: name, Body: data})
		}
	}
	if len(files) == 0 {
		return nil, errors.New("archive contains no *.conf files")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return convert("archive", files, all, o)
}

type server struct {
	d       *conf.Directive
	file    string
	names   []string
	ssl     bool
	http2   bool
	ports   []int
	redirTo bool // 只做 http→https 跳转
}

type conv struct {
	o         Options
	r         *Report
	archive   map[string][]byte
	upstreams map[string]string // 原名 → 规范名
	servers   []*server
	httpSnip  []*conf.Directive
	autoUps   map[string]string // "host:port" → 上游名
}

func convert(source string, files []conf.File, archive map[string][]byte, o Options) (*Report, error) {
	c := &conv{o: o, r: &Report{Source: source, Sites: []SiteResult{}, Upstreams: []UpstreamResult{}, Global: []Issue{}, Certificates: map[string]CertMaterial{}},
		archive: archive, upstreams: map[string]string{}, autoUps: map[string]string{}}
	parsed, issues, err := expandIncludes(source, files, archive)
	if err != nil {
		return nil, err
	}
	c.r.Global = append(c.r.Global, issues...)
	// 先收集全部 upstream（站点可能引用其他文件中的上游）
	for _, ds := range parsed {
		c.walk(ds, true)
	}
	for _, ds := range parsed {
		c.walk(ds, false)
	}
	c.buildSites()
	if len(c.httpSnip) > 0 {
		out, issues, err := render.ValidateDirectives(render.ScopeHTTP, c.httpSnip, render.SnippetContext{})
		switch {
		case err != nil:
			c.global(c.httpSnip[0], "http 片段未导入："+err.Error())
		case len(issues) > 0:
			c.global(c.httpSnip[0], "http 片段未导入："+issues[0].Message)
		default:
			c.r.HTTPSnippet = out
		}
	}
	c.summarize()
	return c.r, nil
}

func (c *conv) global(d *conf.Directive, reason string) {
	c.r.Global = append(c.r.Global, Issue{File: d.File, Line: d.Line, Directive: d.Name, Reason: reason})
}

// walk 遍历文件顶层；upstreamsOnly 为真时只收集 upstream。
func (c *conv) walk(ds []*conf.Directive, upstreamsOnly bool) {
	for _, d := range ds {
		switch d.Name {
		case "http":
			c.walkHTTP(d.Block, upstreamsOnly)
		case "upstream":
			if upstreamsOnly {
				c.upstream(d)
			}
		case "server":
			if !upstreamsOnly {
				c.addServer(d) // conf.d 风格：文件顶层直接是 server
			}
		default:
			if upstreamsOnly {
				continue
			}
			switch d.Name {
			case "stream":
				c.global(d, "四层代理（stream）将在 P9 提供，未导入")
			case "events", "mail":
				c.global(d, "全局配置由平台管理，未导入")
			case "include":
				c.global(d, "未找到 include 引用的文件或匹配项，请核对上传内容："+strings.Join(d.Args, " "))
			default:
				c.global(d, "全局指令由平台管理，未导入")
			}
		}
	}
}

func (c *conv) walkHTTP(ds []*conf.Directive, upstreamsOnly bool) {
	for _, d := range ds {
		switch d.Name {
		case "upstream":
			if upstreamsOnly {
				c.upstream(d)
			}
		case "server":
			if !upstreamsOnly {
				c.addServer(d)
			}
		default:
			if upstreamsOnly {
				continue
			}
			switch d.Name {
			case "include":
				c.global(d, "未找到 include 引用的文件或匹配项，请核对上传内容："+strings.Join(d.Args, " "))
			case "log_format", "access_log", "error_log", "sendfile", "tcp_nopush", "tcp_nodelay", "server_tokens", "types",
				"default_type", "ssl_protocols", "ssl_ciphers", "ssl_prefer_server_ciphers", "ssl_session_cache", "ssl_session_timeout", "resolver":
				c.global(d, "由平台基线配置统一管理，未导入")
			default:
				if _, issues, err := render.ValidateDirectives(render.ScopeHTTP, []*conf.Directive{cloneDirective(d)}, render.SnippetContext{}); err == nil && len(issues) == 0 {
					c.httpSnip = append(c.httpSnip, d)
				} else {
					reason := "http 层不支持此指令"
					if len(issues) > 0 {
						reason = issues[0].Message
					}
					c.global(d, reason+"，需人工处理")
				}
			}
		}
	}
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

func normName(s string) string {
	n := strings.Trim(invalidNameChars.ReplaceAllString(strings.ToLower(strings.ReplaceAll(s, "_", "-")), "-"), "-")
	if len(n) > 63 {
		n = n[:63]
	}
	if len(n) < 2 {
		n = "x-" + n
	}
	return n
}

func (c *conv) upstream(d *conf.Directive) {
	if len(d.Args) != 1 || d.Block == nil {
		c.global(d, "upstream 格式无法识别")
		return
	}
	name := normName(d.Args[0])
	u := render.UpstreamGroup{Name: name}
	ok := true
	for _, x := range d.Block {
		switch x.Name {
		case "least_conn", "ip_hash":
			u.Algorithm = x.Name
		case "server":
			if len(x.Args) == 0 {
				ok = false
				continue
			}
			s := render.UpstreamServer{Addr: x.Args[0]}
			for _, p := range x.Args[1:] {
				if strings.HasPrefix(p, "weight=") {
					s.Weight, _ = strconv.Atoi(strings.TrimPrefix(p, "weight="))
				} else {
					c.global(x, fmt.Sprintf("上游 %s 的服务器参数 %s 未导入", d.Args[0], p))
				}
			}
			if err := render.ValidateUpstream(s.Addr, c.o.AllowedNet); err != nil {
				c.global(x, fmt.Sprintf("上游 %s 的地址 %s 不是允许回源网段内的 IP:端口（主机名或网段未登记），需人工处理", d.Args[0], s.Addr))
				ok = false
				continue
			}
			u.Servers = append(u.Servers, s)
		default:
			c.global(x, fmt.Sprintf("上游 %s 的指令 %s 未导入", d.Args[0], x.Name))
		}
	}
	if !ok || len(u.Servers) == 0 {
		c.global(d, fmt.Sprintf("上游 %s 未导入（地址需人工处理）；引用它的路由也需人工处理", d.Args[0]))
		return
	}
	c.upstreams[d.Args[0]] = name
	c.r.Upstreams = append(c.r.Upstreams, UpstreamResult{Spec: u, File: d.File, Line: d.Line})
}

func (c *conv) addServer(d *conf.Directive) {
	s := &server{d: d, file: d.File}
	for _, x := range d.Block {
		switch x.Name {
		case "listen":
			for _, a := range x.Args {
				switch a {
				case "ssl":
					s.ssl = true
				case "http2":
					s.http2 = true
				default:
					if p := listenPort(a); p > 0 {
						s.ports = append(s.ports, p)
					}
				}
			}
		case "server_name":
			s.names = append(s.names, x.Args...)
		case "http2":
			if len(x.Args) == 1 && x.Args[0] == "on" {
				s.http2 = true
			}
		}
	}
	for _, p := range s.ports {
		if p == 443 {
			s.ssl = s.ssl || hasDirective(d.Block, "ssl_certificate")
		}
	}
	s.redirTo = onlyHTTPSRedirect(d.Block)
	c.servers = append(c.servers, s)
	c.r.Summary.Servers++
}

func listenPort(a string) int {
	if i := strings.LastIndex(a, ":"); i >= 0 {
		a = a[i+1:]
	}
	p, err := strconv.Atoi(a)
	if err != nil {
		return 0
	}
	return p
}

func hasDirective(ds []*conf.Directive, name string) bool {
	for _, d := range ds {
		if d.Name == name {
			return true
		}
	}
	return false
}

// onlyHTTPSRedirect：server 中除了 listen / server_name 之外只有一条跳转到 https 的 return（或等价 location /）。
func onlyHTTPSRedirect(ds []*conf.Directive) bool {
	found := false
	for _, d := range ds {
		switch d.Name {
		case "listen", "server_name":
		case "return":
			if len(d.Args) == 2 && (d.Args[0] == "301" || d.Args[0] == "308") && strings.HasPrefix(d.Args[1], "https://") {
				found = true
			} else {
				return false
			}
		case "location":
			if len(d.Args) == 1 && d.Args[0] == "/" && len(d.Block) == 1 && d.Block[0].Name == "return" && onlyHTTPSRedirect(d.Block) {
				found = true
			} else {
				return false
			}
		default:
			return false
		}
	}
	return found
}

func (c *conv) buildSites() {
	used := map[string]bool{}
	// 按域名集合配对：只做 https 跳转的 80 server 与同域名的 443 server 合并为一个站点
	key := func(s *server) string {
		n := append([]string(nil), s.names...)
		sort.Strings(n)
		return strings.Join(n, " ")
	}
	redirects := map[string]*server{}
	for _, s := range c.servers {
		if s.redirTo && !s.ssl {
			redirects[key(s)] = s
		}
	}
	merged := map[*server]bool{}
	for _, s := range c.servers {
		if s.ssl {
			if r, ok := redirects[key(s)]; ok {
				merged[r] = true
				c.site(s, true, used)
			}
		}
	}
	for _, s := range c.servers {
		if merged[s] {
			continue
		}
		if s.ssl {
			if _, ok := redirects[key(s)]; ok {
				continue // 已作为合并站点处理
			}
		}
		c.site(s, false, used)
	}
}

func (c *conv) issue(sr *SiteResult, d *conf.Directive, reason string, blocking bool) {
	sr.Issues = append(sr.Issues, Issue{File: d.File, Line: d.Line, Directive: d.Name, Reason: reason, Blocking: blocking})
}

var baselineHeaders = map[string]string{"host": "$host", "x-real-ip": "$remote_addr", "x-forwarded-for": "$proxy_add_x_forwarded_for", "x-forwarded-proto": "$scheme", "x-request-id": "$request_id", "connection": ""}

func (c *conv) site(s *server, forceHTTPS bool, used map[string]bool) {
	sr := SiteResult{File: s.file, Line: s.d.Line, LocationSnippets: map[string]string{}, Issues: []Issue{}}
	var domains []string
	for _, n := range s.names {
		ln := strings.ToLower(n)
		if err := render.ValidateDomain(ln); err != nil {
			c.issue(&sr, &conf.Directive{Name: "server_name", File: s.file, Line: s.d.Line}, fmt.Sprintf("域名 %q 不支持（默认站点、正则或裸通配），未导入", n), false)
			continue
		}
		domains = append(domains, ln)
	}
	sr.Spec = render.Site{Domains: domains, ForceHTTPS: forceHTTPS, HTTP2: s.http2 && s.ssl}
	if len(domains) == 0 {
		sr.Name = fmt.Sprintf("server-%d", s.d.Line)
		sr.Status = StatusFailed
		c.issue(&sr, s.d, "没有可用的域名，无法创建站点", true)
		c.r.Sites = append(c.r.Sites, sr)
		return
	}
	base := normName(strings.SplitN(strings.TrimPrefix(domains[0], "*."), ".", 2)[0])
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	sr.Name = name
	ctx := render.SnippetContext{Site: name, Upstreams: map[string]bool{}, AllowedNet: c.o.AllowedNet}
	for _, u := range c.upstreams {
		ctx.Upstreams[u] = true
	}
	var serverSnip []*conf.Directive
	for _, x := range s.d.Block {
		switch x.Name {
		case "listen":
			for _, p := range listenPorts(x) {
				if p != 80 && p != 443 {
					c.issue(&sr, x, fmt.Sprintf("端口 %d 不支持（站点只监听 80 / 443）", p), true)
				}
			}
		case "server_name", "http2":
		case "ssl_certificate":
			if len(x.Args) == 1 {
				sr.CertFile = x.Args[0]
			}
		case "ssl_certificate_key":
			if len(x.Args) == 1 {
				sr.KeyFile = x.Args[0]
			}
		case "ssl_protocols", "ssl_ciphers", "ssl_prefer_server_ciphers", "ssl_session_cache", "ssl_session_timeout", "ssl_session_tickets", "ssl_stapling", "ssl_stapling_verify":
			c.issue(&sr, x, "TLS 参数由平台基线统一管理，未导入", false)
		case "access_log", "error_log":
			c.issue(&sr, x, "日志由平台统一管理，未导入", false)
		case "index":
			c.issue(&sr, x, "已忽略（静态文件需人工迁移）", false)
		case "include":
			c.issue(&sr, x, "未找到 include 引用的文件或匹配项，请核对上传内容："+strings.Join(x.Args, " "), true)
		case "location":
			c.location(&sr, x, ctx, &serverSnip)
		case "return":
			if forceHTTPS || s.redirTo {
				c.issue(&sr, x, "http→https 跳转已转换为“强制 HTTPS”", false)
				sr.Spec.ForceHTTPS = true
				continue
			}
			c.toServerSnippet(&sr, x, ctx, &serverSnip)
		default:
			c.toServerSnippet(&sr, x, ctx, &serverSnip)
		}
	}
	if s.ssl {
		sr.Spec.HTTPS = &render.TLS{}
		sr.CertName = name
		if sr.CertFile == "" || sr.KeyFile == "" {
			c.issue(&sr, s.d, "HTTPS 站点缺少 ssl_certificate / ssl_certificate_key", true)
		} else if crt, key := c.findFile(sr.CertFile), c.findFile(sr.KeyFile); crt != nil && key != nil {
			sr.CertFound = true
			c.r.Certificates[name] = CertMaterial{CertPEM: string(crt), KeyPEM: string(key)}
		} else {
			c.issue(&sr, &conf.Directive{Name: "ssl_certificate", File: s.file, Line: s.d.Line},
				"证书文件不在上传内容中：请在证书库上传后关联到该站点", true)
		}
	} else if sr.Spec.ForceHTTPS {
		sr.Spec.ForceHTTPS = false
		c.issue(&sr, s.d, "只有 http 跳转、没有对应的 HTTPS server，未开启强制 HTTPS", true)
	}
	if len(serverSnip) > 0 {
		out, issues, err := render.ValidateDirectives(render.ScopeServer, serverSnip, ctx)
		switch {
		case err != nil:
			c.issue(&sr, s.d, "站点片段未导入："+err.Error(), true)
		case len(issues) > 0:
			c.issue(&sr, s.d, "站点片段未导入："+issues[0].Message, true)
		default:
			sr.ServerSnippet = out
		}
	}
	// 用渲染器的模型校验检查结果（例如路由冲突）
	in := render.Input{Revision: "r000000", AllowedNet: c.o.AllowedNet}
	for _, u := range c.r.Upstreams {
		in.Upstreams = append(in.Upstreams, u.Spec)
	}
	chk := sr.Spec
	chk.Name = name
	if m, ok := c.r.Certificates[name]; ok && chk.HTTPS != nil {
		chk.HTTPS = &render.TLS{CertPEM: m.CertPEM, KeyPEM: m.KeyPEM}
	} else {
		chk.HTTPS, chk.ForceHTTPS, chk.HTTP2 = nil, false, false // 证书待上传：其余部分照常校验
	}
	in.Sites = []render.Site{chk}
	if err := in.Validate(); err != nil {
		c.issue(&sr, s.d, "转换结果未通过校验："+err.Error(), true)
	}
	switch {
	case len(sr.Spec.Routes) == 0 && sr.ServerSnippet == "" && sr.Blocking():
		sr.Status = StatusFailed
	case len(sr.Issues) == 0 && sr.ServerSnippet == "" && len(sr.LocationSnippets) == 0:
		sr.Status = StatusFull
	default:
		onlyInfo := true
		for _, i := range sr.Issues {
			if i.Blocking {
				onlyInfo = false
			}
		}
		if onlyInfo && sr.ServerSnippet == "" && len(sr.LocationSnippets) == 0 {
			sr.Status = StatusFull
		} else {
			sr.Status = StatusPartial
		}
	}
	c.r.Sites = append(c.r.Sites, sr)
}

func listenPorts(x *conf.Directive) []int {
	var out []int
	for _, a := range x.Args {
		if p := listenPort(a); p > 0 {
			out = append(out, p)
		}
	}
	return out
}

// toServerSnippet：白名单内的放进 server 片段，否则需人工处理。
func (c *conv) toServerSnippet(sr *SiteResult, x *conf.Directive, ctx render.SnippetContext, acc *[]*conf.Directive) {
	_, issues, err := render.ValidateDirectives(render.ScopeServer, []*conf.Directive{cloneDirective(x)}, ctx)
	if err == nil && len(issues) == 0 {
		*acc = append(*acc, x)
		return
	}
	for _, i := range issues {
		file := i.File
		if file == "" {
			file = x.File
		}
		sr.Issues = append(sr.Issues, Issue{File: file, Line: i.Line, Directive: i.Directive, Reason: i.Message + "，需人工处理", Blocking: true})
	}
	if err != nil {
		c.issue(sr, x, err.Error(), true)
	}
}

// location 转换为路由；转不了路由时整块作为 server 片段（仍需白名单内），否则需人工处理。
func (c *conv) location(sr *SiteResult, x *conf.Directive, ctx render.SnippetContext, acc *[]*conf.Directive) {
	issuesBefore := len(sr.Issues)
	defer func() {
		location := "location（特殊匹配，请查看原文件）"
		if len(x.Args) == 1 && render.ValidatePath(x.Args[0]) == nil {
			location = "location " + x.Args[0]
		} else if len(x.Args) == 2 && (x.Args[0] == "=" || x.Args[0] == "^~") && render.ValidatePath(x.Args[1]) == nil {
			location = "location " + x.Args[0] + " " + x.Args[1]
		}
		for i := issuesBefore; i < len(sr.Issues); i++ {
			sr.Issues[i].Location = location
		}
	}()
	r, rest, ok := c.route(x, sr.Name)
	if ok {
		if len(rest) > 0 {
			out, issues, err := render.ValidateDirectives(render.ScopeLocation, rest, ctx)
			if err != nil || len(issues) > 0 {
				ok = false // 路由内有不允许的指令：退回整块处理，报告对应行
			} else {
				sr.LocationSnippets[r.Path] = out
			}
		}
		if ok {
			sr.Spec.Routes = append(sr.Spec.Routes, r)
			return
		}
		delete(sr.LocationSnippets, r.Path)
	}
	before := len(*acc)
	c.toServerSnippet(sr, x, ctx, acc)
	if len(*acc) > before {
		c.issue(sr, x, "该路由转为站点片段，请审批片段并核对后启用站点", true)
	}
}

// route 尝试把 location 映射为路由，返回路由与需要放进 location 片段的其余指令。
func (c *conv) route(x *conf.Directive, site string) (render.Route, []*conf.Directive, bool) {
	var r render.Route
	switch {
	case len(x.Args) == 1:
		r.Path, r.Match = x.Args[0], "prefix"
	case len(x.Args) == 2 && x.Args[0] == "=":
		r.Path, r.Match = x.Args[1], "exact"
	default:
		return r, nil, false // 正则及 ^~ 优先级无法表示为普通路由
	}
	if render.ValidatePath(r.Path) != nil {
		return r, nil, false
	}
	// 仅在三个必需指令都明确且没有重复时转换 WebSocket。动态 map、继承与其他取值需人工核对。
	wsCandidate, upgrade, connection, connectionTotal, httpVersion, badVersion := false, 0, 0, 0, 0, false
	for _, d := range x.Block {
		if d.Name == "proxy_set_header" && len(d.Args) > 0 {
			switch strings.ToLower(d.Args[0]) {
			case "upgrade":
				wsCandidate = true
				if len(d.Args) != 2 || d.Args[1] != "$http_upgrade" {
					return r, nil, false
				}
				upgrade++
			case "connection":
				if len(d.Args) != 2 {
					return r, nil, false
				}
				connectionTotal++
				if d.Args[1] != "" {
					wsCandidate = true
				}
				if strings.EqualFold(d.Args[1], "upgrade") {
					connection++
				} else if d.Args[1] != "" {
					return r, nil, false
				}
			}
		}
		if d.Name == "proxy_http_version" {
			if len(d.Args) != 1 || d.Args[0] != "1.1" {
				badVersion = true
			}
			httpVersion++
		}
	}
	if wsCandidate && (upgrade != 1 || connection != 1 || connectionTotal != 1 || httpVersion != 1 || badVersion) {
		return r, nil, false
	}
	var rest []*conf.Directive
	for _, d := range x.Block {
		switch d.Name {
		case "proxy_pass":
			if r.Mode != "" || len(d.Args) != 1 {
				return r, nil, false
			}
			up, ok := c.proxyUpstream(d.Args[0], site)
			if !ok {
				return r, nil, false
			}
			r.Mode, r.Proxy = "proxy", &render.ProxySpec{Upstream: up}
		case "return":
			if r.Mode != "" {
				return r, nil, false
			}
			switch {
			case len(d.Args) == 1 && d.Args[0] == "403":
				r.Mode = "deny"
			case len(d.Args) == 2:
				code, _ := strconv.Atoi(d.Args[0])
				r.Mode, r.Redirect = "redirect", &render.RedirectSpec{Code: code, To: d.Args[1]}
				if (&render.Input{Revision: "r000000", Sites: []render.Site{{Name: "x0", Domains: []string{"x.example.com"}, Routes: []render.Route{r}}}}).Validate() != nil {
					return r, nil, false
				}
			default:
				return r, nil, false
			}
		case "deny":
			if r.Mode != "" || len(d.Args) != 1 || d.Args[0] != "all" {
				return r, nil, false
			}
			r.Mode = "deny"
		case "proxy_set_header":
			if len(d.Args) >= 1 {
				if wsCandidate && (strings.EqualFold(d.Args[0], "upgrade") || strings.EqualFold(d.Args[0], "connection")) {
					continue
				}
				if expected, ok := baselineHeaders[strings.ToLower(d.Args[0])]; ok {
					if len(d.Args) != 2 || d.Args[1] != expected {
						return r, nil, false // 不得静默改变原有代理头（例如 WebSocket）
					}
					continue
				}
			}
			rest = append(rest, d)
		case "proxy_http_version":
			if !wsCandidate {
				rest = append(rest, d)
			}
		default:
			rest = append(rest, d)
		}
	}
	if r.Mode == "" {
		return r, nil, false
	}
	if wsCandidate {
		if r.Proxy == nil {
			return r, nil, false
		}
		r.Proxy.WebSocket = true
	}
	if r.Mode != "proxy" {
		for _, d := range rest {
			if strings.HasPrefix(d.Name, "proxy_") {
				return r, nil, false
			}
		}
	}
	return r, rest, true
}

// proxyUpstream：只映射不带 URI 的 HTTP 上游；末尾斜杠也可能改变转发路径。
func (c *conv) proxyUpstream(target, site string) (string, bool) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.RawQuery != "" || strings.Contains(target, "$") {
		return "", false
	}
	if n, ok := c.upstreams[u.Host]; ok {
		return n, true
	}
	if render.ValidateUpstream(u.Host, c.o.AllowedNet) != nil {
		return "", false
	}
	if n, ok := c.autoUps[u.Host]; ok {
		return n, true
	}
	n := normName(fmt.Sprintf("%s-%d", site, len(c.autoUps)+1))
	c.autoUps[u.Host] = n
	c.upstreams[u.Host] = n
	c.r.Upstreams = append(c.r.Upstreams, UpstreamResult{Spec: render.UpstreamGroup{Name: n, Servers: []render.UpstreamServer{{Addr: u.Host}}}, Auto: true})
	return n, true
}

// findFile 在压缩包中按路径后缀查找配置引用的文件（至少匹配文件名与上一级目录，或文件名唯一）。
func (c *conv) findFile(ref string) []byte {
	if c.archive == nil {
		return nil
	}
	parts := strings.Split(path.Clean(ref), "/")
	best, bestN, ties := "", 0, 0
	for name := range c.archive {
		np := strings.Split(name, "/")
		n := 0
		for i := 1; i <= len(parts) && i <= len(np) && parts[len(parts)-i] == np[len(np)-i]; i++ {
			n++
		}
		switch {
		case n > bestN:
			best, bestN, ties = name, n, 1
		case n == bestN && n > 0:
			ties++
		}
	}
	if bestN > 0 && ties == 1 {
		return c.archive[best]
	}
	return nil
}

func cloneDirective(d *conf.Directive) *conf.Directive {
	cp := *d
	cp.Args = append([]string(nil), d.Args...)
	if d.Block != nil {
		cp.Block = make([]*conf.Directive, len(d.Block))
		for i, b := range d.Block {
			cp.Block[i] = cloneDirective(b)
		}
	}
	return &cp
}

func (c *conv) summarize() {
	s := &c.r.Summary
	s.Sites = len(c.r.Sites)
	s.Issues = len(c.r.Global)
	for _, x := range c.r.Sites {
		s.Issues += len(x.Issues)
		switch x.Status {
		case StatusFull:
			s.Full++
		case StatusPartial:
			s.Partial++
		default:
			s.Failed++
		}
	}
}
