package importer

import (
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// Included files must be interpreted at their include site, not as unrelated
// top-level files. The same file can be included by more than one server.
const nestedIncludeDump = `# configuration file /etc/web/web.conf:
http {
  include conf.d/*.conf;
  server { listen 80; server_name alpha.example.com; include /etc/web/routes.conf; }
  server { listen 80; server_name beta.example.com; include /etc/web/routes.conf; }
}
# configuration file /etc/web/conf.d/upstreams.conf:
upstream backend { server 10.0.1.11:8080; }
# configuration file /etc/web/routes.conf:
location /api/ { include fragments/proxy.inc; }
include fragments/extra.conf;
location ~ \.php$ { fastcgi_pass 127.0.0.1:9000; }
# configuration file /etc/web/fragments/proxy.inc:
proxy_pass http://backend;
proxy_read_timeout 12s;
# configuration file /etc/web/fragments/extra.conf:
location /private { deny all; }
# configuration file /etc/web/unreferenced.conf:
server { listen 80; server_name accidental.example.com; location / { deny all; } }
`

func TestSeparateFilesResolveAbsoluteIncludes(t *testing.T) {
	files := []conf.File{
		{Name: "web.conf", Body: []byte(`http { server { listen 80; server_name files.example.com; include /srv/old/conf/first.conf; include /srv/old/conf/second.conf; } }`)},
		{Name: "first.conf", Body: []byte(`location /one { deny all; }`)},
		{Name: "second.conf", Body: []byte(`location /two { deny all; }`)},
	}
	r, err := FromFiles(files, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "files")
	if s == nil || len(s.Spec.Routes) != 2 || s.Blocking() {
		t.Fatalf("separate files not resolved: %+v", s)
	}
}

func TestIncludedProxyHeadersAndURIAreNotSilentlyChanged(t *testing.T) {
	dump := `# configuration file /etc/web/web.conf:
http { server { listen 80; server_name headers.example.com; include headers.conf; } }
# configuration file /etc/web/headers.conf:
location /socket { proxy_pass http://10.0.1.11:8080; proxy_set_header Connection "upgrade"; }
location /api/ { proxy_pass http://10.0.1.11:8080/; }
`
	r, err := FromDump([]byte(dump), opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "headers")
	if s == nil {
		t.Fatal("site missing")
	}
	if len(s.Spec.Routes) != 0 {
		t.Fatalf("routes with different proxy semantics must not be auto-converted: %+v", s.Spec.Routes)
	}
	if !s.Blocking() && (!strings.Contains(s.ServerSnippet, "upgrade") || !strings.Contains(s.ServerSnippet, "8080/")) {
		t.Fatalf("proxy semantics must be preserved or explicitly blocked: %+v", s)
	}
}

func TestPrefixPriorityIsPreservedOrReported(t *testing.T) {
	dump := `# configuration file /etc/web/web.conf:
http { server { listen 80; server_name priority.example.com; location / { deny all; } include routes.conf; } }
# configuration file /etc/web/routes.conf:
location ^~ /special/ { deny all; }
`
	r, err := FromDump([]byte(dump), opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "priority")
	if s == nil {
		t.Fatal("site missing")
	}
	for _, route := range s.Spec.Routes {
		if route.Path == "/special/" {
			t.Fatalf("^~ priority was silently converted to ordinary prefix: %+v", route)
		}
	}
	if !s.Blocking() || !strings.Contains(s.ServerSnippet, "^~") {
		t.Fatalf("a route requiring snippet approval must be kept disabled until reviewed: %+v", s)
	}
}

func TestMultipleMainConfigurationsAreNotGuessed(t *testing.T) {
	a := tgz(t, map[string]string{
		"first.conf":  `http { server { listen 80; server_name one.example.com; location / { deny all; } } }`,
		"second.conf": `http { server { listen 80; server_name two.example.com; location / { deny all; } } }`,
	})
	if _, err := FromArchive(a, opts); err == nil {
		t.Fatal("multiple main configurations must not be guessed")
	}
}

func TestCombinedSnippetLimitsAreReported(t *testing.T) {
	dump := "# configuration file /etc/web/web.conf:\nhttp { server { listen 80; server_name limit.example.com; location / { deny all; }\n" +
		strings.Repeat("client_max_body_size 8m;\n", 301) + "} }\n"
	r, err := FromDump([]byte(dump), opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "limit")
	if s == nil || !s.Blocking() {
		t.Fatalf("an oversized combined snippet must not be silently dropped: %+v", s)
	}
}

func TestDumpExpandsIncludesInTheirOriginalContext(t *testing.T) {
	r, err := FromDump([]byte(nestedIncludeDump), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.Servers != 2 || len(r.Sites) != 2 || find(r, "accidental") != nil {
		t.Fatalf("unexpected sites: %+v", r.Sites)
	}
	for _, name := range []string{"alpha", "beta"} {
		s := find(r, name)
		if s == nil || len(s.Spec.Routes) != 2 || s.Spec.Routes[0].Path != "/api/" || s.Spec.Routes[1].Path != "/private" {
			t.Fatalf("%s: included routes not attributed to server: %+v", name, s)
		}
		if s.Spec.Routes[0].Proxy == nil || s.Spec.Routes[0].Proxy.Upstream != "backend" || !strings.Contains(s.LocationSnippets["/api/"], "proxy_read_timeout") {
			t.Fatalf("%s: nested location include lost: %+v", name, s)
		}
		if !s.Blocking() {
			t.Fatalf("%s: unsupported regex route must block publication", name)
		}
		found := false
		for _, issue := range s.Issues {
			if issue.Directive == "fastcgi_pass" && issue.File == "/etc/web/routes.conf" && issue.Line == 3 {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: missing source location for unsupported directive: %+v", name, s.Issues)
		}
	}
}

func TestArchiveExpandsAbsoluteIncludeFromArchivedPaths(t *testing.T) {
	a := tgz(t, map[string]string{
		"web/conf/syedge.conf":   `http { server { listen 80; server_name archive.example.com; include /srv/original/web/conf/routes/*.conf; } }`,
		"web/conf/routes/a.conf": `location /first { deny all; }`,
		"web/conf/routes/b.conf": `location /second { deny all; }`,
	})
	r, err := FromArchive(a, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "archive")
	if s == nil || len(s.Spec.Routes) != 2 || s.Spec.Routes[0].Path != "/first" || s.Spec.Routes[1].Path != "/second" || s.Blocking() {
		t.Fatalf("archive include not expanded in order: %+v", s)
	}
}

func TestMissingIncludeBlocksAffectedSite(t *testing.T) {
	dump := `# configuration file /etc/web/web.conf:
http { server { listen 80; server_name missing.example.com; location / { deny all; } include /etc/web/missing.conf; } }
`
	r, err := FromDump([]byte(dump), opts)
	if err != nil {
		t.Fatal(err)
	}
	s := find(r, "missing")
	if s == nil || !s.Blocking() || len(s.Spec.Routes) != 1 {
		t.Fatalf("missing include was not blocked: %+v", s)
	}
	for _, issue := range s.Issues {
		if issue.Directive == "include" && strings.Contains(issue.Reason, "missing.conf") && issue.Blocking {
			return
		}
	}
	t.Fatalf("missing include not explained: %+v", s.Issues)
}

func TestCyclicIncludesAreRejected(t *testing.T) {
	dump := `# configuration file /etc/web/web.conf:
http { server { listen 80; server_name cycle.example.com; include a.conf; } }
# configuration file /etc/web/a.conf:
include b.conf;
# configuration file /etc/web/b.conf:
include a.conf;
`
	if _, err := FromDump([]byte(dump), opts); err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("cyclic include must be rejected: %v", err)
	}
}

func TestArchiveAmbiguousIncludeIsNotGuessed(t *testing.T) {
	a := tgz(t, map[string]string{
		"main.conf":            `http { server { listen 80; server_name ambiguous.example.com; location / { deny all; } include /original/conf/routes.conf; } }`,
		"one/conf/routes.conf": `location /one { deny all; }`,
		"two/conf/routes.conf": `location /two { deny all; }`,
	})
	r, err := FromArchive(a, opts)
	if err != nil {
		return // Rejecting an ambiguous archive is also safe.
	}
	s := find(r, "ambiguous")
	if s == nil || !s.Blocking() || len(s.Spec.Routes) != 1 {
		t.Fatalf("ambiguous include must not be guessed: %+v", s)
	}
}

func TestArchiveAmbiguousCertificatesAreNotGuessed(t *testing.T) {
	c := conv{archive: map[string][]byte{
		"/a/certs/site.crt": []byte("first"),
		"/b/certs/site.crt": []byte("second"),
	}}
	if got := c.findFile("/old/certs/site.crt"); got != nil {
		t.Fatalf("ambiguous certificate must not be selected: %q", got)
	}
}
