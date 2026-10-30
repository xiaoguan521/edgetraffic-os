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
