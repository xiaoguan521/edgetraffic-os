package render

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T, host string) *TLS {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	return &TLS{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})),
	}
}

func sampleInput(t *testing.T) Input {
	return Input{
		Revision:   "r000002",
		AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12")},
		RealIPFrom: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		MimeTypes:  []byte("types {\n    text/html  html;\n}\n"),
		Upstreams: []UpstreamGroup{{
			Name: "api", Algorithm: "least_conn",
			Servers: []UpstreamServer{{Addr: "10.0.1.11:8080", Weight: 2}, {Addr: "10.0.1.12:8080"}},
		}},
		Sites: []Site{
			{
				Name: "shop", Domains: []string{"shop.example.com", "www.shop.example.com"},
				HTTPS: selfSigned(t, "shop.example.com"), ForceHTTPS: true, HTTP2: true,
				Routes: []Route{
					{Path: "/", Match: "prefix", Mode: "static", Static: &StaticSpec{IndexHTML: "<h1>shop</h1>"}},
					{Path: "/api/", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "api"}},
					{Path: "/old", Match: "exact", Mode: "redirect", Redirect: &RedirectSpec{To: "https://shop.example.com/", Code: 301}},
					{Path: "/admin/", Match: "prefix", Mode: "deny"},
				},
			},
			{
				Name: "blog", Domains: []string{"blog.example.com"},
				Routes: []Route{{Path: "/", Match: "prefix", Mode: "static", Static: &StaticSpec{IndexHTML: "<h1>blog</h1>"}}},
			},
		},
	}
}

func TestRenderDeterministic(t *testing.T) {
	in := sampleInput(t)
	a, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.Digest != b.Manifest.Digest {
		t.Fatal("same input produced different digests")
	}
	// 路由顺序不影响结果
	in.Sites[0].Routes[0], in.Sites[0].Routes[3] = in.Sites[0].Routes[3], in.Sites[0].Routes[0]
	c, _ := Render(in)
	if c.Manifest.Digest != a.Manifest.Digest {
		t.Fatal("route order changed digest")
	}
	for _, f := range []string{"syedge.conf", "mime.types", "snippets/security.conf", "conf.d/00-upstreams.conf",
		"conf.d/site-shop.conf", "conf.d/site-blog.conf", "certs/site-shop.crt", "certs/site-shop.key", ManifestName} {
		if _, ok := a.Files[f]; !ok {
			t.Errorf("missing %s", f)
		}
	}
	site := string(a.Files["conf.d/site-shop.conf"])
	for _, want := range []string{"return  301  https://$host$request_uri;", "http2        on;", "proxy_pass        http://up_api;", "return  403;", "location = /old"} {
		if !strings.Contains(site, want) {
			t.Errorf("site config missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(site), "ngi"+"nx") {
		t.Error("brand leaked into rendered config")
	}
}

// 规格书 6.5：渲染器必测用例（模型层）。
func TestRenderRejects(t *testing.T) {
	mut := []struct {
		name string
		f    func(in *Input)
	}{
		{"traversal path", func(in *Input) { in.Sites[1].Routes[0].Path = "/api/..%2f..%2fetc/passwd" }},
		{"injection path", func(in *Input) { in.Sites[1].Routes[0].Path = "/a;return 200 x" }},
		{"bare wildcard", func(in *Input) { in.Sites[1].Domains = []string{"*"} }},
		{"metadata upstream", func(in *Input) { in.Upstreams[0].Servers[0].Addr = "169.254.169.254:80" }},
		{"unregistered upstream", func(in *Input) { in.Upstreams[0].Servers[0].Addr = "192.168.9.9:80" }},
		{"unknown upstream ref", func(in *Input) { in.Sites[0].Routes[1].Proxy.Upstream = "nope" }},
		{"duplicate domain", func(in *Input) { in.Sites[1].Domains = []string{"shop.example.com"} }},
		{"duplicate route", func(in *Input) { in.Sites[1].Routes = append(in.Sites[1].Routes, in.Sites[1].Routes[0]) }},
		{"bad redirect", func(in *Input) { in.Sites[0].Routes[2].Redirect.To = "https://x.com/; return 200" }},
		{"redirect scheme", func(in *Input) { in.Sites[0].Routes[2].Redirect.To = "javascript:alert(1)" }},
		{"static prefix no slash", func(in *Input) { in.Sites[1].Routes[0].Path = "/docs" }},
		{"http2 without https", func(in *Input) { in.Sites[1].HTTP2 = true }},
		{"bad revision", func(in *Input) { in.Revision = "r1/../x" }},
		{"bad site name", func(in *Input) { in.Sites[1].Name = "Blog_1" }},
		{"stream reserved port", func(in *Input) {
			in.Streams = []StreamProxy{{Name: "x1", Listen: 443, Servers: []UpstreamServer{{Addr: "10.0.1.21:3306"}}}}
		}},
		{"stream ssrf", func(in *Input) {
			in.Streams = []StreamProxy{{Name: "x1", Listen: 3306, Servers: []UpstreamServer{{Addr: "169.254.169.254:80"}}}}
		}},
		{"stream duplicate port", func(in *Input) {
			in.Streams = []StreamProxy{{Name: "x1", Listen: 3306, Servers: []UpstreamServer{{Addr: "10.0.1.21:3306"}}},
				{Name: "x2", Listen: 3306, Servers: []UpstreamServer{{Addr: "10.0.1.21:3306"}}}}
		}},
		{"stream sni on udp", func(in *Input) {
			in.Streams = []StreamProxy{{Name: "x1", Listen: 53, Protocol: "udp", Servers: []UpstreamServer{{Addr: "10.0.1.21:53"}},
				SNIRoutes: []SNIRoute{{Domain: "a.example.com", Servers: []UpstreamServer{{Addr: "10.0.1.21:53"}}}}}}
		}},
	}
	for _, m := range mut {
		in := sampleInput(t)
		m.f(&in)
		if _, err := Render(in); err == nil {
			t.Errorf("%s: accepted", m.name)
		}
	}
}

// 用真实 syedge 二进制做语法校验；未安装时跳过（CI 的 dataplane 任务中会安装）。
func TestRenderSyedgeSyntax(t *testing.T) {
	bin := os.Getenv("SYEDGE_BIN")
	if bin == "" {
		bin = "/opt/syedge/sbin/syedge"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syedge binary not installed")
	}
	in := sampleInput(t)
	if mt, err := os.ReadFile("/opt/syedge/share/defaults/mime.types"); err == nil {
		in.MimeTypes = mt
	}
	// 带一个进行中的 ACME 挑战，确认挑战 location 也能通过真实 syedge -t
	tok := "abcDEF0123456789_-tokenXYZ"
	in.AcmeChallenges = []AcmeChallenge{{Token: tok, KeyAuth: tok + ".thumbprintABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"}}
	// 三个插入点的自定义片段（12.7）也要通过真实 syedge -t
	in.HTTPSnippets = []NamedSnippet{{Name: "tuning", Text: `gzip_comp_level 5; gzip_types text/css application/json;
limit_req_zone $binary_remote_addr zone=api_rl:10m rate=10r/s;
map $http_x_env $env_tag { default prod; ~^test test; }`}}
	in.Sites[0].ServerSnippet = `client_max_body_size 50m;
add_header X-Env $env_tag always;
location /v2/ { proxy_pass http://up_api/v2/; proxy_set_header X-Env $env_tag; proxy_read_timeout 120s; }
if ($request_method = TRACE) { return 405; }`
	for i := range in.Sites[0].Routes {
		if in.Sites[0].Routes[i].Mode == "proxy" {
			in.Sites[0].Routes[i].Snippet = `limit_req zone=api_rl burst=20 nodelay; proxy_read_timeout 300s; proxy_set_header X-Tenant "a b";`
		}
	}
	// 静态资源包路由也要通过真实 syedge -t
	in.Sites[1].Routes[0].Static.Files = map[string][]byte{"index.html": []byte("<h1>x</h1>"), "css/a.css": []byte("a{}")}
	// 四层代理（含 SNI 分流与 UDP）也要通过真实 syedge -t
	in.Streams = []StreamProxy{
		{Name: "mysql", Listen: 3306, Servers: []UpstreamServer{{Addr: "10.0.1.21:3306"}}, Algorithm: "least_conn", ProxyProtocol: true},
		{Name: "tls-sni", Listen: 8443, Servers: []UpstreamServer{{Addr: "10.0.1.22:443"}},
			SNIRoutes: []SNIRoute{{Domain: "a.example.com", Servers: []UpstreamServer{{Addr: "10.0.1.23:443"}}}}},
		{Name: "dns", Listen: 5353, Protocol: "udp", Servers: []UpstreamServer{{Addr: "10.0.1.24:53"}}, Algorithm: "hash"},
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), in.Revision)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	// 测试环境不一定有 syedge 用户和日志目录：用 -g 覆盖 user 之外的项不可行，
	// 因此只在 root 下运行完整校验。
	if os.Geteuid() != 0 {
		t.Skip("syedge -t needs root to check user and log paths")
	}
	out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syedge.conf")).CombinedOutput()
	if err != nil {
		t.Fatalf("syedge -t failed: %v\n%s", err, out)
	}
	if _, err := b.TarGz(); err != nil {
		t.Fatal(err)
	}
}

func TestStaticBundle(t *testing.T) {
	in := sampleInput(t)
	in.Sites[1].Routes[0].Static.Files = map[string][]byte{"index.html": []byte("<h1>docs</h1>"), "css/app.css": []byte("body{}"), "img/a.png": {0x89, 'P'}}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := path.Join("sites", in.Sites[1].Name, routeDir(in.Sites[1].Routes[0]))
	if string(b.Files[path.Join(dir, "css/app.css")]) != "body{}" || string(b.Files[path.Join(dir, "index.html")]) != "<h1>docs</h1>" {
		t.Fatalf("static files missing: %v", b.Manifest.Files)
	}
	for _, bad := range []string{"../etc/passwd", "/abs", ".git/config", "a/../b", "a b", "a;b",
		"a/.well-known/x", ".well-known/.hidden", ".well-known/../x", ".well-knownx/a"} {
		in := sampleInput(t)
		in.Sites[1].Routes[0].Static.Files = map[string][]byte{bad: []byte("x")}
		if _, err := Render(in); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	// 根目录 .well-known/ 是唯一允许的点开头目录
	for _, ok := range []string{".well-known/security.txt", ".well-known/apple-app-site-association"} {
		if err := ValidateStaticPath(ok); err != nil {
			t.Errorf("path %q rejected: %v", ok, err)
		}
	}
	// 纯资源包（没有 index.html、也没有内联页面）也能渲染
	in = sampleInput(t)
	in.Sites[1].Routes[0].Static = &StaticSpec{Files: map[string][]byte{"files/a.pdf": []byte("%PDF")}}
	b, err = Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Files[path.Join(dir, "index.html")]; ok {
		t.Fatal("asset-only bundle must not get a generated index.html")
	}
}
