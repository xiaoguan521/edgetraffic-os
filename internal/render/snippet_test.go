package render

import (
	"net/netip"
	"strings"
	"testing"
)

var snipCtx = SnippetContext{
	Site:       "shop",
	Upstreams:  map[string]bool{"api": true},
	AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
}

func TestSnippetAllowed(t *testing.T) {
	cases := map[SnippetScope]string{
		ScopeHTTP: `gzip on; gzip_types text/css application/json;
limit_req_zone $binary_remote_addr zone=api_rl:10m rate=10r/s;
map $http_x_env $env_backend { default prod; ~^test test; }`,
		ScopeServer: `client_max_body_size 50m;
add_header X-Env prod always;
location /v2/ {
    proxy_pass http://up_api/v2/;
    proxy_read_timeout 120s;
}
location = /old { return 301 /new; }
if ($request_method = TRACE) { return 405; }`,
		ScopeLocation: `proxy_read_timeout 300s;
proxy_buffering off;
expires 1h;
rewrite ^/a/(.*)$ /b/$1 break;
sub_filter 'http://' 'https://';`,
	}
	for scope, src := range cases {
		out, issues, err := ValidateSnippet(scope, src, snipCtx)
		if err != nil || len(issues) > 0 {
			t.Fatalf("%s: %v %+v", scope, err, issues)
		}
		if out == "" || strings.Contains(out, "\t") {
			t.Fatalf("%s: bad normalized output %q", scope, out)
		}
	}
}

func TestSnippetRejected(t *testing.T) {
	cases := []struct {
		scope SnippetScope
		src   string
		want  string
	}{
		{ScopeServer, `include /etc/passwd;`, "include"},
		{ScopeHTTP, `load_module modules/x.so;`, "load_module"},
		{ScopeLocation, `access_by_lua_file /tmp/x.lua;`, "access_by_lua_file"},
		{ScopeServer, `perl_modules /tmp;`, "perl_modules"},
		{ScopeServer, `ssi on;`, "ssi"},
		{ScopeLocation, `autoindex on;`, "autoindex"},
		{ScopeServer, `location /x { alias /etc/; }`, "alias"},
		{ScopeServer, `location /x { root /opt/syapp/conf/current/sites/other/; }`, "root"},
		{ScopeServer, `location /x { root /opt/syapp/conf/current/sites/shop/../../; }`, "root"},
		{ScopeServer, `location /x { proxy_pass http://169.254.169.254/; }`, "proxy_pass"},
		{ScopeServer, `location /x { proxy_pass http://up_unknown; }`, "proxy_pass"},
		{ScopeServer, `location /x { proxy_pass http://$host; }`, "proxy_pass"},
		{ScopeLocation, `proxy_pass http://up_api;`, "proxy_pass"},
		{ScopeServer, `add_header Server evil;`, "add_header"},
		{ScopeServer, `add_header x-frame-options DENY;`, "add_header"},
		{ScopeLocation, `add_header X-A b;`, "add_header"},
		{ScopeLocation, `proxy_set_header Transfer-Encoding chunked;`, "proxy_set_header"},
		{ScopeLocation, `proxy_set_header Host evil;`, "proxy_set_header"},
		{ScopeLocation, `if ($x) { proxy_pass http://up_api; }`, "if"},
		{ScopeHTTP, `server { listen 80; }`, "server"},
		{ScopeLocation, `location /nested { return 200; }`, "location"},
		{ScopeHTTP, `proxy_cache_path /tmp/x keys_zone=a:1m;`, "proxy_cache_path"},
		{ScopeServer, `listen 8080;`, "listen"},
	}
	for _, c := range cases {
		_, issues, err := ValidateSnippet(c.scope, c.src, snipCtx)
		if err != nil {
			t.Fatalf("%q: parse error %v", c.src, err)
		}
		if len(issues) == 0 || issues[0].Directive != c.want || issues[0].Line == 0 {
			t.Errorf("%s %q: want issue on %s, got %+v", c.scope, c.src, c.want, issues)
		}
	}
}

func TestSnippetParseErrorAndLimits(t *testing.T) {
	if _, _, err := ValidateSnippet(ScopeServer, "gzip on", snipCtx); err == nil {
		t.Fatal("unterminated directive should be a parse error")
	}
	if _, _, err := ValidateSnippet(ScopeServer, strings.Repeat("gzip on;\n", 3000), snipCtx); err == nil {
		t.Fatal("oversized snippet should be rejected")
	}
	if _, _, err := ValidateSnippet("bogus", "gzip on;", snipCtx); err == nil {
		t.Fatal("unknown scope")
	}
}

func TestSnippetNestedProxyGetsBaselineHeaders(t *testing.T) {
	out, issues, err := ValidateSnippet(ScopeServer, `location /v2/ { proxy_pass http://up_api; proxy_set_header X-Env a; }`, snipCtx)
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	for _, h := range []string{"Host", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Proto", "X-Request-ID", "X-Env"} {
		if !strings.Contains(out, "proxy_set_header  "+h+" ") {
			t.Errorf("missing %s in\n%s", h, out)
		}
	}
}

func TestSnippetNormalizationNeutralizesQuotes(t *testing.T) {
	// 引号里的 ; } 不能逃出指令：输出重新解析后仍是同一条指令
	out, issues, err := ValidateSnippet(ScopeLocation, `return 200 "ok; } server { listen 1;";`, snipCtx)
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	if strings.Count(out, ";\n") != 1 || !strings.HasPrefix(out, "return  200  \"") {
		t.Fatalf("normalized: %q", out)
	}
}

func TestRenderWithSnippets(t *testing.T) {
	in := Input{Revision: "r000001", MimeTypes: []byte("types {}"),
		AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		Upstreams:  []UpstreamGroup{{Name: "api", Servers: []UpstreamServer{{Addr: "10.0.0.11:8080"}}}},
		Sites: []Site{{Name: "shop", Domains: []string{"shop.example.com"},
			Routes:        []Route{{Path: "/api/", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "api"}, Snippet: "proxy_read_timeout 300s;"}},
			ServerSnippet: "client_max_body_size 50m;"}},
		HTTPSnippets: []NamedSnippet{{Name: "gzip", Text: "gzip on;"}},
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	site := string(b.Files["conf.d/site-shop.conf"])
	if !strings.Contains(site, "        proxy_read_timeout  300s;") || !strings.Contains(site, "    client_max_body_size  50m;") {
		t.Fatalf("site conf:\n%s", site)
	}
	if string(b.Files["conf.d/snippet-gzip.conf"]) == "" {
		t.Fatal("http snippet file missing")
	}
	// 渲染时再次校验：绕过控制台直接塞进 Input 的危险片段同样被拒绝
	in.Sites[0].ServerSnippet = "include /etc/passwd;"
	if _, err := Render(in); err == nil {
		t.Fatal("render must reject a forbidden snippet")
	}
	in.Sites[0].ServerSnippet = ""
	in.HTTPSnippets[0].Name = "../x"
	if _, err := Render(in); err == nil {
		t.Fatal("render must reject a bad snippet name")
	}
}
