package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// 一份“-T”输出：主配置 + conf.d 下两个站点 + 一个四层代理，覆盖完全转换、部分转换与无法转换三种情况。
const dump = `# configuration file /etc/web/web.conf:
user www;
worker_processes auto;
load_module modules/ngx_stream_module.so;
events { worker_connections 1024; }
http {
    include mime.types;
    gzip on;
    upstream backend {
        least_conn;
        server 10.0.1.11:8080 weight=3;
        server 10.0.1.12:8080;
    }
    upstream legacy { server app.internal:9000; }
    include conf.d/*.conf;
}
stream {
    server { listen 3306; proxy_pass 10.0.2.2:3306; }
}

# configuration file /etc/web/conf.d/shop.conf:
server {
    listen 80;
    server_name shop.example.com www.shop.example.com;
    return 301 https://$host$request_uri;
}
server {
    listen 443 ssl;
    http2 on;
    server_name shop.example.com www.shop.example.com;
    ssl_certificate     /etc/web/certs/shop.crt;
    ssl_certificate_key /etc/web/certs/shop.key;
    client_max_body_size 20m;
    location /api/ {
        proxy_pass http://backend;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 120s;
    }
    location = /old {
        return 302 https://shop.example.com/new;
    }
    location /admin/ {
        deny all;
    }
}

# configuration file /etc/web/conf.d/blog.conf:
server {
    listen 80;
    server_name blog.example.com;
    location / {
        proxy_pass http://10.0.3.3:8080;
    }
    location ~ \.php$ {
        fastcgi_pass 127.0.0.1:9000;
    }
    location /files/ {
        root /data/files;
        autoindex on;
    }
}
`

var opts = Options{AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}

func find(r *Report, name string) *SiteResult {
	for i := range r.Sites {
		if r.Sites[i].Name == name {
			return &r.Sites[i]
		}
	}
	return nil
}

func TestImportDump(t *testing.T) {
	r, err := FromDump([]byte(dump), opts)
	if err != nil {
		t.Fatal(err)
	}
	// 上游：backend 转换成功；legacy 用主机名，需人工处理
	if len(r.Upstreams) != 2 || r.Upstreams[1].Auto != true || r.Upstreams[0].Spec.Name != "backend" || r.Upstreams[0].Spec.Algorithm != "least_conn" || r.Upstreams[0].Spec.Servers[0].Weight != 3 {
		t.Fatalf("upstreams %+v", r.Upstreams)
	}
	// shop：80 跳转 server 合并为 force_https；三条路由全部转换；client_max_body_size 与 proxy_read_timeout 转为片段
	shop := find(r, "shop")
	if shop == nil {
		t.Fatalf("sites %+v", r.Sites)
	}
	if shop.Status != StatusPartial || !shop.Spec.ForceHTTPS || !shop.Spec.HTTP2 || shop.Spec.HTTPS == nil || len(shop.Spec.Routes) != 3 {
		t.Fatalf("shop %+v", shop)
	}
	if !strings.Contains(shop.ServerSnippet, "client_max_body_size") || !strings.Contains(shop.LocationSnippets["/api/"], "proxy_read_timeout") {
		t.Fatalf("shop snippets %q %v", shop.ServerSnippet, shop.LocationSnippets)
	}
	// -T 输出里没有证书文件：唯一需人工处理的是证书
	for _, i := range shop.Issues {
		if i.Blocking && i.Directive != "ssl_certificate" {
			t.Fatalf("unexpected blocking issue %+v", i)
		}
	}
	if !shop.Blocking() {
		t.Fatal("missing certificate must block the site")
	}
	if shop.CertFile != "/etc/web/certs/shop.crt" || shop.CertFound {
		t.Fatalf("cert ref %+v", shop)
	}
	// blog：php 正则 location 与 autoindex 无法转换，必须人工处理
	blog := find(r, "blog")
	if blog == nil || !blog.Blocking() || blog.Status != StatusPartial {
		t.Fatalf("blog %+v", blog)
	}
	var lines []string
	for _, i := range blog.Issues {
		if i.File != "/etc/web/conf.d/blog.conf" || i.Line == 0 {
			t.Fatalf("issue without file/line: %+v", i)
		}
		lines = append(lines, i.Directive)
	}
	j := strings.Join(lines, ",")
	if !strings.Contains(j, "fastcgi_pass") || !strings.Contains(j, "autoindex") {
		t.Fatalf("blog issues %s", j)
	}
	// 直连地址的 proxy_pass 自动生成上游组
	if blog.Spec.Routes[0].Proxy == nil || !strings.HasPrefix(blog.Spec.Routes[0].Proxy.Upstream, "blog-") {
		t.Fatalf("blog route %+v", blog.Spec.Routes)
	}
	// 全局与 stream 中的内容全部出现在报告里，一行不丢
	g := strings.Join(func() []string {
		var s []string
		for _, i := range r.Global {
			s = append(s, i.Directive+" "+i.Reason)
		}
		return s
	}(), ",")
	for _, want := range []string{"load_module", "stream", "user", "worker_processes", "legacy"} {
		if !strings.Contains(g, want) {
			t.Errorf("global report misses %s: %s", want, g)
		}
	}
	// 报告可以序列化（下载）
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	if r.Summary.Servers != 3 || r.Summary.Full+r.Summary.Partial+r.Summary.Failed != 2 {
		t.Fatalf("summary %+v", r.Summary)
	}
}

func TestImportInventoriesLegacyLuaAssets(t *testing.T) {
	r, err := FromFiles([]conf.File{
		{Name: "conf/syedge.conf", Body: []byte(`http { server { listen 80; server_name lua.example.com; location / { access_by_lua_file lua/access.lua; proxy_pass http://10.0.0.2:8080; } } }`)},
		{Name: "lua/access.lua", Body: []byte("return true\n")},
		{Name: "lua/lualib/resty/example.lua", Body: []byte("return {}\n")},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Lua.RequiresLegacy || len(r.Lua.References) != 1 || len(r.Lua.Assets) != 2 {
		t.Fatalf("legacy Lua inventory = %+v", r.Lua)
	}
	ref := r.Lua.References[0]
	if ref.Directive != "access_by_lua_file" || ref.Target != "lua/access.lua" || ref.Resolved != "/lua/access.lua" || ref.Missing {
		t.Fatalf("Lua reference = %+v", ref)
	}
	if r.Lua.Assets[0].Path != "/lua/access.lua" || r.Lua.Assets[0].SHA256 == "" || r.Lua.Assets[1].Path != "/lua/lualib/resty/example.lua" {
		t.Fatalf("Lua assets = %+v", r.Lua.Assets)
	}
}

func TestLuaReferenceReportsUncertainPathsAndDependencies(t *testing.T) {
	r, err := FromFiles([]conf.File{
		{Name: "conf/syedge.conf", Body: []byte(`http { server { listen 80; server_name lua.example.com; location / { access_by_lua_file lua/secure/access.lua; proxy_pass http://10.0.0.2:8080; } } }`)},
		{Name: "lua/other/access.lua", Body: []byte("local mysql = require('resty.mysql')\n")},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	ref := r.Lua.References[0]
	if ref.Resolved != "" || ref.Candidate != "/lua/other/access.lua" || ref.Resolution != "unverified" {
		t.Fatalf("same-name script was reported as an exact match: %+v", ref)
	}
	if len(r.Lua.Dependencies) != 1 || r.Lua.Dependencies[0].Target != "resty.mysql" || r.Lua.Dependencies[0].Status != "unverified" {
		t.Fatalf("Lua dependency was not reported as unverified: %+v", r.Lua.Dependencies)
	}
}

func TestLuaDumpWithoutFilesDoesNotClaimVerified(t *testing.T) {
	r, err := FromDump([]byte("# configuration file /tmp/syedge.conf:\nhttp { init_by_lua_file lua/init.lua; }\n"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lua.References) != 1 || r.Lua.References[0].Resolution != "unverified" {
		t.Fatalf("dump-only reference needs verification: %+v", r.Lua.References)
	}
}

func selfSigned(t *testing.T, cn string) (string, string) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	kb, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func tgz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(b))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestImportArchiveWithCertificates(t *testing.T) {
	crt, key := selfSigned(t, "shop.example.com")
	shop := strings.SplitN(strings.SplitN(dump, "# configuration file /etc/web/conf.d/shop.conf:\n", 2)[1], "# configuration file", 2)[0]
	main := strings.SplitN(strings.SplitN(dump, "# configuration file /etc/web/web.conf:\n", 2)[1], "# configuration file", 2)[0]
	arch := tgz(t, map[string]string{
		"web/web.conf":         main,
		"web/conf.d/shop.conf": shop,
		"web/certs/shop.crt":   crt,
		"web/certs/shop.key":   key,
	})
	r, err := FromArchive(arch, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "shop")
	if s == nil || !s.CertFound || s.Blocking() {
		t.Fatalf("cert not matched or still blocking: %+v", s)
	}
	c := r.Certificates[s.CertName]
	if c.CertPEM == "" || c.KeyPEM == "" {
		t.Fatal("certificate material missing")
	}
	// 私钥只在 Certificates 里（不序列化），报告 JSON 中不出现
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE KEY") || strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatal("report leaks certificate material")
	}
}

func TestArchiveLimits(t *testing.T) {
	if _, err := FromArchive([]byte("not a gzip"), opts); err == nil {
		t.Fatal("bad archive accepted")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../evil.conf", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if _, err := FromArchive(buf.Bytes(), opts); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestImportSyEdgeBaselineSkipsInternalBlocksAndPreservesUpstreamOptions(t *testing.T) {
	base, err := render.BaselineFor(render.Profile{Mode: "system"})
	if err != nil {
		t.Fatalf("BaselineFor: %v", err)
	}
	appConf := `upstream api_pool {
    least_conn;
    keepalive 64;
    server 10.0.1.10:8080 weight=3 max_fails=5 fail_timeout=30s;
    server 10.0.1.11:8080 backup;
    server 10.0.1.12:8080 down;
}

server {
    listen 80;
    server_name api.example.com;
    location / {
        proxy_pass http://api_pool;
    }
}
`
	arch := tgz(t, map[string]string{
		"syedge.conf":     string(base.Files["syedge.conf"]),
		"mime.types":      string(base.Files["mime.types"]),
		"conf.d/app.conf": appConf,
	})
	r, err := FromArchive(arch, opts)
	if err != nil {
		t.Fatalf("FromArchive: %v", err)
	}
	// 平台默认断开 server（80/443 server_name _）与 127.0.0.1 状态页应被静默过滤，只剩 1 个业务站点。
	if r.Summary.Servers != 1 || r.Summary.Full != 1 || r.Summary.Partial != 0 || r.Summary.Failed != 0 {
		t.Fatalf("expected 1 full site and 0 baseline noise, got summary=%+v sites=%+v", r.Summary, r.Sites)
	}
	// 平台基线 http 指令（limit_req_zone edge_per_ip、默认超时与头）不得落入 httpSnip，避免发布冲突。
	if strings.TrimSpace(r.HTTPSnippet) != "" {
		t.Fatalf("expected empty http snippet for unmodified baseline, got:\n%s", r.HTTPSnippet)
	}
	if len(r.Upstreams) != 1 {
		t.Fatalf("expected 1 upstream, got %+v", r.Upstreams)
	}
	u := r.Upstreams[0].Spec
	if u.Name != "api-pool" || u.Algorithm != "least_conn" || u.Keepalive != 64 {
		t.Fatalf("unexpected upstream header: %+v", u)
	}
	if len(u.Servers) != 3 {
		t.Fatalf("expected 3 servers, got %+v", u.Servers)
	}
	if u.Servers[0].Weight != 3 || u.Servers[0].MaxFails != 5 || u.Servers[0].FailTimeout != 30 {
		t.Fatalf("server[0] options not preserved: %+v", u.Servers[0])
	}
	if !u.Servers[1].Backup || u.Servers[1].Down {
		t.Fatalf("server[1] backup flag not preserved: %+v", u.Servers[1])
	}
	if !u.Servers[2].Down || u.Servers[2].Backup {
		t.Fatalf("server[2] down flag not preserved: %+v", u.Servers[2])
	}
}

func TestImportCustomNodeGroupPortsAndUserModifiedHTTPDirective(t *testing.T) {
	base, err := render.BaselineFor(render.Profile{Mode: "user", Root: "/home/ops/syedge", HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 18081})
	if err != nil {
		t.Fatalf("BaselineFor user mode: %v", err)
	}
	// 模拟运维在主配置里调大了 client_max_body_size 为 50m（非基线默认 1m），应保留进 http 片段。
	mainConf := strings.Replace(string(base.Files["syedge.conf"]), "client_max_body_size   1m;", "client_max_body_size   50m;", 1)
	crt, key := selfSigned(t, "portal.example.com")
	siteConf := `server {
    listen 8080;
    server_name portal.example.com;
    return 301 https://$host$request_uri;
}
server {
    listen 8443 ssl;
    server_name portal.example.com;
    ssl_certificate certs/portal.crt;
    ssl_certificate_key certs/portal.key;
    location / {
        proxy_pass http://10.0.2.5:9000;
    }
}
`
	arch := tgz(t, map[string]string{
		"syedge.conf":        mainConf,
		"mime.types":         string(base.Files["mime.types"]),
		"conf.d/portal.conf": siteConf,
		"certs/portal.crt":   crt,
		"certs/portal.key":   key,
	})
	customOpts := Options{
		AllowedNet: opts.AllowedNet,
		HTTPPort:   8080,
		HTTPSPort:  8443,
	}
	r, err := FromArchive(arch, customOpts)
	if err != nil {
		t.Fatalf("FromArchive: %v", err)
	}
	s := find(r, "portal")
	if s == nil {
		t.Fatalf("portal site missing: %+v", r.Sites)
	}
	if s.Status != StatusFull || s.Blocking() || !s.Spec.ForceHTTPS || !s.CertFound {
		t.Fatalf("expected full non-blocking site on 8080/8443, got %+v", s)
	}
	if !strings.Contains(r.HTTPSnippet, "client_max_body_size") || !strings.Contains(r.HTTPSnippet, "50m") {
		t.Fatalf("user-customized client_max_body_size 50m should be kept in http snippet, got %q", r.HTTPSnippet)
	}
	if strings.Contains(r.HTTPSnippet, "edge_per_ip") {
		t.Fatalf("platform baseline limit_req_zone leaked into http snippet: %q", r.HTTPSnippet)
	}
}

func TestImportDoesNotDropMixedLoopbackServerAndReportsInvalidUpstreamParams(t *testing.T) {
	cfg := `http {
    upstream bad_params {
        keepalive 2000;
        server 10.0.1.20:8080 weight=200 max_fails=0 fail_timeout=0s;
    }
    server {
        listen 127.0.0.1:80;
        server_name internal.example.com;
        location = /status {
            stub_status;
        }
        location /api/ {
            proxy_pass http://bad_params;
        }
    }
}
`
	r, err := FromFiles([]conf.File{{Name: "syedge.conf", Body: []byte(cfg)}}, opts)
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	// 混用 stub_status 与业务 proxy_pass 的本机 server 不能被当作平台状态页静默丢弃。
	if r.Summary.Servers != 1 || find(r, "internal") == nil {
		t.Fatalf("mixed loopback business server was unexpectedly dropped: summary=%+v sites=%+v", r.Summary, r.Sites)
	}
	if len(r.Upstreams) != 1 {
		t.Fatalf("expected 1 upstream, got %+v", r.Upstreams)
	}
	u := r.Upstreams[0].Spec
	if u.Keepalive != 0 || u.Servers[0].Weight != 0 || u.Servers[0].MaxFails != 0 || u.Servers[0].FailTimeout != 0 {
		t.Fatalf("out-of-range upstream parameters should not be stored: %+v", u)
	}
	var reasons []string
	for _, g := range r.Global {
		reasons = append(reasons, g.Reason)
	}
	joined := strings.Join(reasons, " | ")
	for _, want := range []string{"keepalive", "weight=200", "max_fails=0", "fail_timeout=0s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected global warning mentioning %q, got %q", want, joined)
		}
	}
}
