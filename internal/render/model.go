package render

import (
	"encoding/pem"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// Input 是一次渲染的全部输入：一个节点组的完整期望状态。
type Input struct {
	Revision   string          // ^r[0-9]{6,12}$
	AllowedNet []netip.Prefix  // 允许回源网段
	RealIPFrom []netip.Prefix  // CDN / LB 回源网段；为空则不输出 real_ip 配置
	Sites      []Site          // 站点
	Upstreams  []UpstreamGroup // 上游组
	MimeTypes  []byte          // 由数据面安装包的 share/defaults/mime.types 提供

	AcmeChallenges []AcmeChallenge // 进行中的 ACME HTTP-01 挑战，渲染进 snippets/acme-challenge.conf

	HTTPSnippets []NamedSnippet // 已审批的 http 层自定义片段（12.7）
	Streams      []StreamProxy  // 四层代理（P9，授权功能 stream）
	IPSets       []IPSet        // 节点组的 IP 集 / 地域库（P10，被规则的 ip_set 条件引用）

	Profile Profile // 节点组的实例档案（路径与端口）；零值为默认实例
}

// AcmeChallenge 是一个 HTTP-01 挑战：节点对 /.well-known/acme-challenge/<Token> 返回 KeyAuth。
type AcmeChallenge struct {
	Token   string `json:"token"`
	KeyAuth string `json:"key_auth"`
}

// Site 是一个站点（规格书 6.1，MVP 子集）。
type Site struct {
	Name       string   `json:"name"`
	Domains    []string `json:"domains"`
	HTTPS      *TLS     `json:"https,omitempty"`
	ForceHTTPS bool     `json:"force_https"`
	HTTP2      bool     `json:"http2"`
	Routes     []Route  `json:"routes"`
	Rules      []Rule   `json:"rules,omitempty"` // 访问规则（P10 规则积木，rules.go）

	ServerSnippet string `json:"-"` // 已审批的 server 层自定义片段（不随站点表单保存，由控制台按审批状态填入）
}

// TLS 证书以 PEM 原文传入，渲染进版本目录 certs/。
// 控制台中站点只保存 CertID（证书库编号），渲染前由控制台解析为 PEM；PEM 字段不落库。
type TLS struct {
	CertID  uint   `json:"cert_id,omitempty"`
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
	// GMCertID 引用证书库中的国密 SM2 双证书（签名 + 加密，TLCP）。只能下发到国密变体（铜锁构建）的节点；
	// 标准证书仍然保留，服务不支持国密的客户端。渲染前由控制台解析为 GM。
	GMCertID uint    `json:"gm_cert_id,omitempty"`
	GM       *GMPair `json:"-"`
}

// GMPair 是国密 TLCP 双证书的 PEM 原文（只在渲染时存在，不落库）。
type GMPair struct {
	SignCertPEM, SignKeyPEM string
	EncCertPEM, EncKeyPEM   string
}

// pemOnly 报告 s 是否只由 PEM 块组成：证书文件为一个或多个 CERTIFICATE，私钥文件恰好一个 *PRIVATE KEY（不带口令）。
func pemOnly(s string, cert bool) bool {
	rest := []byte(strings.TrimSpace(s))
	n := 0
	for len(rest) > 0 {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return false
		}
		rest = []byte(strings.TrimSpace(string(rest)))
		n++
		if cert && b.Type != "CERTIFICATE" || !cert && (!strings.HasSuffix(b.Type, "PRIVATE KEY") || strings.Contains(b.Type, "ENCRYPTED")) {
			return false
		}
	}
	return n > 0 && (cert || n == 1)
}

// HasGM 报告是否有站点启用了国密双证书。
func (in Input) HasGM() bool {
	for _, s := range in.Sites {
		if s.HTTPS != nil && s.HTTPS.GM != nil {
			return true
		}
	}
	return false
}

// Route 是站点内的一条路由。
type Route struct {
	Path     string        `json:"path"`
	Match    string        `json:"match"` // prefix | exact
	Mode     string        `json:"mode"`  // static | proxy | redirect | deny
	Static   *StaticSpec   `json:"static,omitempty"`
	Proxy    *ProxySpec    `json:"proxy,omitempty"`
	Redirect *RedirectSpec `json:"redirect,omitempty"`

	Snippet string `json:"-"` // 已审批的 location 层自定义片段
	// LuaScripts 是已审批的自定义 Lua 脚本（执行阶段 → 脚本），由控制台按审批状态填入（luascript.go）
	LuaScripts map[string]string `json:"-"`
}

// StaticSpec：MVP 中静态内容随配置版本一起发布（单个首页，最大 256 KiB）。
type StaticSpec struct {
	IndexHTML string `json:"index_html"`
	// BundleID 引用控制台的静态资源包（P9 静态资源发布）；渲染前由控制台解析为 Files，随配置版本一起原子切换
	BundleID uint              `json:"bundle_id,omitempty"`
	Files    map[string][]byte `json:"-"`
}

// 静态资源包的限制（整个版本包上限 64 MiB，为配置留出余量）。
const (
	MaxStaticBytes = 48 << 20
	MaxStaticFiles = 5000
)

var staticPathRe = regexp.MustCompile(`^[A-Za-z0-9._~@+-]+(/[A-Za-z0-9._~@+-]+)*$`)

// ValidateStaticPath 校验静态资源包中的相对路径（不允许穿越、隐藏文件、绝对路径与特殊字符；根目录 .well-known/ 除外）。
func ValidateStaticPath(p string) error {
	if len(p) > 255 || !staticPathRe.MatchString(p) {
		return fmt.Errorf("invalid static file path %q", p)
	}
	for i, seg := range strings.Split(p, "/") {
		if i == 0 && seg == StaticWellKnown {
			continue // 根目录的 .well-known/ 是公开约定目录（RFC 8615）
		}
		if seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("invalid static file path %q", p)
		}
	}
	return nil
}

// StaticWellKnown 是静态资源包中唯一允许的点开头目录，且只能位于根目录。
// security.conf 的点文件禁止规则同样放行它；其下的 acme-challenge/ 由证书签发 location 接管。
const StaticWellKnown = ".well-known"

type ProxySpec struct {
	Upstream  string `json:"upstream"` // UpstreamGroup.Name
	WebSocket bool   `json:"websocket,omitempty"`
}

type RedirectSpec struct {
	To   string `json:"to"`
	Code int    `json:"code"`
}

// UpstreamGroup 是一组回源服务器。
type UpstreamGroup struct {
	Name      string           `json:"name"`
	Algorithm string           `json:"algorithm"` // round_robin | least_conn | ip_hash
	Servers   []UpstreamServer `json:"servers"`
}

type UpstreamServer struct {
	Addr   string `json:"addr"` // ip:port
	Weight int    `json:"weight"`
}

const maxIndexHTML = 256 << 10

var (
	revisionRe = regexp.MustCompile(`^r[0-9]{6,12}$`)
)

// Validate 对输入做结构与语义校验（规格书 6.2 第 1、2 步）。
// ACME token 与账号公钥指纹均为 base64url 字符（RFC 8555 第 8.1、8.3 节）。
var (
	acmeTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	acmeThumbRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,64}$`)
)

func (in *Input) Validate() error {
	if err := in.Profile.Validate(); err != nil {
		return err
	}
	if !revisionRe.MatchString(in.Revision) {
		return fmt.Errorf("render: invalid revision %q", in.Revision)
	}
	for _, c := range in.AcmeChallenges {
		if !acmeTokenRe.MatchString(c.Token) || !strings.HasPrefix(c.KeyAuth, c.Token+".") || !acmeThumbRe.MatchString(c.KeyAuth[len(c.Token)+1:]) {
			return fmt.Errorf("render: invalid ACME challenge")
		}
	}
	ups := map[string]bool{}
	for _, u := range in.Upstreams {
		if err := ValidateName(u.Name); err != nil {
			return fmt.Errorf("upstream: %w", err)
		}
		if ups[u.Name] {
			return fmt.Errorf("upstream %q defined twice", u.Name)
		}
		ups[u.Name] = true
		switch u.Algorithm {
		case "", "round_robin", "least_conn", "ip_hash":
		default:
			return fmt.Errorf("upstream %q: unknown algorithm %q", u.Name, u.Algorithm)
		}
		if len(u.Servers) == 0 {
			return fmt.Errorf("upstream %q has no servers", u.Name)
		}
		for _, s := range u.Servers {
			if err := ValidateUpstream(s.Addr, in.AllowedNet); err != nil {
				return fmt.Errorf("upstream %q: %w", u.Name, err)
			}
			if s.Weight < 0 || s.Weight > 100 {
				return fmt.Errorf("upstream %q: weight out of range", u.Name)
			}
		}
	}

	if err := validateIPSets(in); err != nil {
		return err
	}
	for i := range in.Sites {
		if err := validateRules(in, &in.Sites[i]); err != nil {
			return err
		}
	}

	names := map[string]bool{}
	domains := map[string]string{}
	for _, s := range in.Sites {
		if err := ValidateName(s.Name); err != nil {
			return fmt.Errorf("site: %w", err)
		}
		if names[s.Name] {
			return fmt.Errorf("site %q defined twice", s.Name)
		}
		names[s.Name] = true
		if len(s.Domains) == 0 {
			return fmt.Errorf("site %q has no domains", s.Name)
		}
		for _, d := range s.Domains {
			if err := ValidateDomain(d); err != nil {
				return fmt.Errorf("site %q: %w", s.Name, err)
			}
			if other, ok := domains[d]; ok {
				return fmt.Errorf("domain %q used by sites %q and %q", d, other, s.Name)
			}
			domains[d] = s.Name
		}
		if s.HTTPS != nil {
			if !strings.Contains(s.HTTPS.CertPEM, "BEGIN CERTIFICATE") || !strings.Contains(s.HTTPS.KeyPEM, "PRIVATE KEY") {
				return fmt.Errorf("site %q: invalid certificate or key PEM", s.Name)
			}
			if g := s.HTTPS.GM; g != nil {
				if !pemOnly(g.SignCertPEM, true) || !pemOnly(g.SignKeyPEM, false) || !pemOnly(g.EncCertPEM, true) || !pemOnly(g.EncKeyPEM, false) {
					return fmt.Errorf("site %q: invalid GM certificate or key PEM", s.Name)
				}
			} else if s.HTTPS.GMCertID != 0 {
				return fmt.Errorf("site %q: GM certificate %d not resolved", s.Name, s.HTTPS.GMCertID)
			}
		} else if s.ForceHTTPS || s.HTTP2 {
			return fmt.Errorf("site %q: force_https / http2 require https", s.Name)
		}
		if len(s.Routes) == 0 {
			return fmt.Errorf("site %q has no routes", s.Name)
		}
		seen := map[string]bool{}
		for _, r := range s.Routes {
			if err := ValidatePath(r.Path); err != nil {
				return fmt.Errorf("site %q: %w", s.Name, err)
			}
			if strings.HasPrefix(r.Path, reservedPrefix) {
				return fmt.Errorf("site %q: path prefix %s is reserved", s.Name, reservedPrefix)
			}
			if r.Match != "prefix" && r.Match != "exact" {
				return fmt.Errorf("site %q route %q: match must be prefix or exact", s.Name, r.Path)
			}
			key := r.Match + " " + r.Path
			if seen[key] {
				return fmt.Errorf("site %q: duplicate route %s", s.Name, key)
			}
			seen[key] = true
			if err := r.validateMode(ups); err != nil {
				return fmt.Errorf("site %q route %q: %w", s.Name, r.Path, err)
			}
		}
	}
	return nil
}

func (r *Route) validateMode(ups map[string]bool) error {
	switch r.Mode {
	case "static":
		if r.Static == nil {
			return fmt.Errorf("static spec required")
		}
		if r.Match == "prefix" && !strings.HasSuffix(r.Path, "/") {
			return fmt.Errorf("static prefix route must end with /")
		}
		if len(r.Static.IndexHTML) > maxIndexHTML {
			return fmt.Errorf("index_html larger than %d bytes", maxIndexHTML)
		}
		if r.Static.BundleID != 0 && r.Match != "prefix" {
			return fmt.Errorf("static bundle requires a prefix route")
		}
		total := 0
		if len(r.Static.Files) > MaxStaticFiles {
			return fmt.Errorf("static bundle has more than %d files", MaxStaticFiles)
		}
		for p, b := range r.Static.Files {
			if err := ValidateStaticPath(p); err != nil {
				return err
			}
			total += len(b)
		}
		if total > MaxStaticBytes {
			return fmt.Errorf("static bundle larger than %d bytes", MaxStaticBytes)
		}
	case "proxy":
		if r.Proxy == nil || !ups[r.Proxy.Upstream] {
			return fmt.Errorf("proxy upstream not defined")
		}
	case "redirect":
		if r.Redirect == nil {
			return fmt.Errorf("redirect spec required")
		}
		switch r.Redirect.Code {
		case 301, 302, 307, 308:
		default:
			return fmt.Errorf("redirect code must be 301/302/307/308")
		}
		if err := validateRedirectURL(r.Redirect.To); err != nil {
			return err
		}
	case "deny":
	default:
		return fmt.Errorf("unknown mode %q", r.Mode)
	}
	return nil
}

func validateRedirectURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("redirect target must be an absolute http(s) URL")
	}
	if strings.ContainsAny(s, " \t\r\n;{}'\"`\\$#") {
		return fmt.Errorf("redirect target contains forbidden characters")
	}
	host := u.Hostname()
	if err := ValidateDomain(strings.ToLower(host)); err != nil {
		if _, ipErr := netip.ParseAddr(host); ipErr != nil {
			return fmt.Errorf("redirect target host invalid")
		}
	}
	return nil
}
