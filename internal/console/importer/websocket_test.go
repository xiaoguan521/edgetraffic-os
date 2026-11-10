package importer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

func TestImportWebSocketConservatively(t *testing.T) {
	cases := []struct {
		name, directives string
		converted        bool
	}{
		{"standard", `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade";`, true},
		{"case insensitive headers", `proxy_http_version 1.1; proxy_set_header upgrade $http_upgrade; proxy_set_header connection Upgrade;`, true},
		{"missing connection", `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade;`, false},
		{"missing upgrade", `proxy_http_version 1.1; proxy_set_header Connection upgrade;`, false},
		{"dynamic connection", `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection $connection_upgrade;`, false},
		{"conflicting host", `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection upgrade; proxy_set_header Host $host:443;`, false},
		{"old http version", `proxy_http_version 1.0; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection upgrade;`, false},
		{"duplicate connection", `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection upgrade; proxy_set_header Connection close;`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`events {} http { server { listen 80; server_name socket.example.com; location /socket { proxy_pass http://10.0.1.1:8080; %s } } }`, tc.directives)
			rep, err := FromFiles([]conf.File{{Name: "web.conf", Body: []byte(src)}}, opts)
			if err != nil {
				t.Fatal(err)
			}
			site := rep.Sites[0]
			if tc.converted {
				if len(site.Spec.Routes) != 1 || site.Spec.Routes[0].Proxy == nil || !site.Spec.Routes[0].Proxy.WebSocket || site.Blocking() {
					t.Fatalf("standard WebSocket was not converted: %+v", site)
				}
			} else {
				if !site.Blocking() || len(site.Spec.Routes) != 0 {
					t.Fatalf("unsupported combination must remain a blocking issue: %+v", site)
				}
				found := false
				for _, issue := range site.Issues {
					found = found || strings.Contains(issue.Location, "/socket")
				}
				if !found {
					t.Fatalf("missing location context: %+v", site.Issues)
				}
			}
		})
	}
}
