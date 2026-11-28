package render

import (
	"net/netip"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestAdvancedSiteProxyStaticUpstreamOptions(t *testing.T) {
	in := Input{
		Revision:   "r000010",
		AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		MimeTypes:  []byte("types { text/html html; }\n"),
		Upstreams: []UpstreamGroup{
			{
				Name:      "api-backend",
				Algorithm: "least_conn",
				Keepalive: 64,
				Servers: []UpstreamServer{
					{Addr: "10.0.1.10:8443", Weight: 2, MaxFails: 5, FailTimeout: 30},
					{Addr: "10.0.1.11:8443", Backup: true},
					{Addr: "10.0.1.12:8443", Down: true},
				},
			},
		},
		Sites: []Site{
			{
				Name:              "portal",
				Domains:           []string{"portal.example.com"},
				ClientMaxBodySize: "50m",
				Gzip:              true,
				Routes: []Route{
					{
						Path:   "/",
						Match:  "prefix",
						Mode:   "static",
						Static: &StaticSpec{IndexHTML: "<h1>SPA</h1>", SPA: true},
					},
					{
						Path:  "/api/",
						Match: "prefix",
						Mode:  "proxy",
						Proxy: &ProxySpec{
							Upstream:       "api-backend",
							HTTPS:          true,
							StripPrefix:    true,
							ConnectTimeout: 5,
							ReadTimeout:    120,
							SendTimeout:    60,
							Buffering:      boolPtr(false),
						},
					},
				},
			},
		},
	}

	b, err := Render(in)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	up := string(b.Files["conf.d/00-upstreams.conf"])
	for _, want := range []string{
		"server 10.0.1.10:8443  weight=2  max_fails=5  fail_timeout=30s;",
		"server 10.0.1.11:8443  max_fails=3  fail_timeout=10s  backup;",
		"server 10.0.1.12:8443  max_fails=3  fail_timeout=10s  down;",
		"keepalive 64;",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("upstreams.conf missing %q:\n%s", want, up)
		}
	}

	site := string(b.Files["conf.d/site-portal.conf"])
	for _, want := range []string{
		"client_max_body_size  50m;",
		"gzip  on;",
		"try_files  $uri  $uri/  /index.html;",
		"proxy_ssl_server_name  on;",
		"proxy_ssl_name         $host;",
		"proxy_pass        https://up_api-backend/;",
		"proxy_connect_timeout  5s;",
		"proxy_read_timeout     120s;",
		"proxy_send_timeout     60s;",
		"proxy_buffering        off;",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site-portal.conf missing %q:\n%s", want, site)
		}
	}
}

func TestAdvancedOptionsValidation(t *testing.T) {
	base := func() Input {
		return Input{
			Revision:   "r000010",
			AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
			MimeTypes:  []byte("types {}\n"),
			Upstreams: []UpstreamGroup{
				{Name: "app", Algorithm: "round_robin", Servers: []UpstreamServer{{Addr: "10.0.1.1:8080"}}},
			},
			Sites: []Site{
				{Name: "s1", Domains: []string{"s1.example.com"}, Routes: []Route{{Path: "/api/", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "app"}}}},
			},
		}
	}

	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"invalid body size", func(in *Input) { in.Sites[0].ClientMaxBodySize = "10m; evil" }},
		{"strip_prefix on root", func(in *Input) {
			in.Sites[0].Routes[0].Path = "/"
			in.Sites[0].Routes[0].Proxy.StripPrefix = true
		}},
		{"strip_prefix without trailing slash", func(in *Input) {
			in.Sites[0].Routes[0].Path = "/api"
			in.Sites[0].Routes[0].Proxy.StripPrefix = true
		}},
		{"read_timeout out of range", func(in *Input) { in.Sites[0].Routes[0].Proxy.ReadTimeout = 9999 }},
		{"connect_timeout negative", func(in *Input) { in.Sites[0].Routes[0].Proxy.ConnectTimeout = -1 }},
		{"spa on exact route", func(in *Input) {
			in.Sites[0].Routes[0] = Route{Path: "/index.html", Match: "exact", Mode: "static", Static: &StaticSpec{IndexHTML: "hi", SPA: true}}
		}},
		{"backup with ip_hash", func(in *Input) {
			in.Upstreams[0].Algorithm = "ip_hash"
			in.Upstreams[0].Servers = append(in.Upstreams[0].Servers, UpstreamServer{Addr: "10.0.1.2:8080", Backup: true})
		}},
		{"keepalive out of range", func(in *Input) { in.Upstreams[0].Keepalive = 5000 }},
		{"fail_timeout out of range", func(in *Input) { in.Upstreams[0].Servers[0].FailTimeout = 9999 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}
