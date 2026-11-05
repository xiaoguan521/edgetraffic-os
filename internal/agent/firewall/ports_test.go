package firewall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func TestParseListens(t *testing.T) {
	conf := `
server {
    listen       80  default_server;   # 默认实例
    listen       443  ssl  default_server;
    listen       127.0.0.1:8081;
    listen       [::]:8443 ssl; listen [::1]:9000;
    # listen 1234;
    listen unix:/run/x.sock;
    listen 0.0.0.0:443 quic reuseport;
}
stream { server { listen  13306; } server { listen  5353  udp; } }
`
	got := fmt.Sprint(parseListens(conf))
	want := fmt.Sprint([]listen{{80, false}, {443, false}, {8443, false}, {443, true}, {13306, false}, {5353, true}})
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// 三个实例（默认、blue、未创建完的 green）：放行默认与 blue 的端口及四层代理端口，不含状态页与 green。
func TestListenPortsFromInstances(t *testing.T) {
	root := t.TempDir()
	def, _ := layout.User(root, "")
	inst := func(n string) layout.Layout { l, _ := layout.User(root, n); return l }
	write := func(l layout.Layout, main string, extra map[string]string) {
		dir := filepath.Dir(l.CurrentConf())
		_ = os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755)
		_ = os.MkdirAll(filepath.Join(dir, "stream.d"), 0o755)
		_ = os.WriteFile(l.CurrentConf(), []byte(main), 0o644)
		for f, c := range extra {
			_ = os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644)
		}
	}
	write(def, "http { server { listen 8080 default_server; listen 8443 ssl default_server; listen 127.0.0.1:8081; } }",
		map[string]string{"conf.d/site.conf": "server { listen 8080; listen 8443 ssl; }", "stream.d/mysql.conf": "server { listen 13306; }"})
	blue := inst("blue")
	write(blue, "http { server { listen 18080; listen 18443 ssl; listen 127.0.0.1:18082; } }",
		map[string]string{"stream.d/dns.conf": "server { listen 5353 udp; }"})
	_ = os.WriteFile(blue.InstanceFile(), []byte("{}"), 0o644)
	green := inst("green") // 目录在、instance.json 不在：未创建完，不算
	write(green, "http { server { listen 28080; } }", nil)

	confs := instanceConfs(def, inst)
	if len(confs) != 2 {
		t.Fatalf("confs %v", confs)
	}
	p, ok := ListenPorts(confs)
	if !ok || fmt.Sprint(p.TCP) != "[8080 8443 13306 18080 18443]" || fmt.Sprint(p.UDP) != "[5353]" {
		t.Fatalf("ports %+v ok=%v", p, ok)
	}
	out, err := Render(Options{SSHFrom: []string{"10.0.0.0/8"}, Public: &p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tcp dport { 8080, 8443, 13306, 18080, 18443 } accept") || !strings.Contains(out, "udp dport { 5353 } accept") ||
		strings.Contains(out, "{ 80, 443") {
		t.Fatalf("rules:\n%s", out)
	}
	// 实例监听 SSH 端口：拒绝生成（否则 SSH 对所有来源开放）
	bad := Ports{TCP: []int{22, 80}}
	if _, err := Render(Options{SSHFrom: []string{"10.0.0.0/8"}, Public: &bad}); err == nil {
		t.Fatal("instance listening on the ssh port accepted")
	}
	// 没有任何实例配置：回到默认 80/443
	if _, ok := ListenPorts([]string{filepath.Join(root, "missing.conf")}); ok {
		t.Fatal("missing config reported ok")
	}
}
