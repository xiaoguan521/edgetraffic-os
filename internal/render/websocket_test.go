package render

import (
	"strings"
	"testing"
)

func TestWebSocketRouteHeaders(t *testing.T) {
	in := sampleInput(t)
	in.Sites[0].Routes[1].Proxy.WebSocket = true
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	config := string(b.Files["conf.d/site-shop.conf"])
	for _, want := range []string{"proxy_http_version  1.1;", "proxy_set_header  Upgrade            $http_upgrade;", "proxy_set_header  Connection         \"upgrade\";"} {
		if !strings.Contains(config, want) {
			t.Errorf("WebSocket route missing %q", want)
		}
	}
	if strings.Contains(config, `proxy_set_header  Connection         "";`) {
		t.Fatal("WebSocket route must not also clear the Connection header")
	}
}
