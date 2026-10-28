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
	for _, f := range []string{"syapp.conf", "mime.types", "snippets/security.conf", "conf.d/00-upstreams.conf",
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
	}
	for _, m := range mut {
		in := sampleInput(t)
		m.f(&in)
		if _, err := Render(in); err == nil {
			t.Errorf("%s: accepted", m.name)
		}
	}
}

// 用真实 syapp 二进制做语法校验；未安装时跳过（CI 的 dataplane 任务中会安装）。
func TestRenderSyappSyntax(t *testing.T) {
	bin := os.Getenv("SYAPP_BIN")
	if bin == "" {
		bin = "/opt/syapp/sbin/syapp"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syapp binary not installed")
	}
	in := sampleInput(t)
	if mt, err := os.ReadFile("/opt/syapp/share/defaults/mime.types"); err == nil {
		in.MimeTypes = mt
	}
	// 带一个进行中的 ACME 挑战，确认挑战 location 也能通过真实 syapp -t
	tok := "abcDEF0123456789_-tokenXYZ"
	in.AcmeChallenges = []AcmeChallenge{{Token: tok, KeyAuth: tok + ".thumbprintABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"}}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), in.Revision)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	// 测试环境不一定有 syapp 用户和日志目录：用 -g 覆盖 user 之外的项不可行，
	// 因此只在 root 下运行完整校验。
	if os.Geteuid() != 0 {
		t.Skip("syapp -t needs root to check user and log paths")
	}
	out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syapp.conf")).CombinedOutput()
	if err != nil {
		t.Fatalf("syapp -t failed: %v\n%s", err, out)
	}
	if _, err := b.TarGz(); err != nil {
		t.Fatal(err)
	}
}
