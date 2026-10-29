package firewall

import (
	"strings"
	"testing"
)

func TestRenderDefault(t *testing.T) {
	out, err := Render(Options{SSHFrom: []string{"10.1.0.0/16", "2001:db8::/32", "192.0.2.7"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table inet syapp_fw",
		"policy drop;",
		"ct state established,related accept",
		"ct state invalid drop",
		`iif "lo" accept`,
		"tcp dport { 80, 443 } accept",
		"ip saddr { 10.1.0.0/16, 192.0.2.7/32 } tcp dport 22 accept",
		"ip6 saddr { 2001:db8::/32 } tcp dport 22 accept",
		"icmpv6 type",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// 只管入站：出站不限制（agent 需要主动连接控制台）
	if strings.Contains(out, "hook output") || strings.Contains(out, "hook forward") {
		t.Fatal("must only filter input")
	}
	// 自己的表：先建再删，重复执行不叠加规则，也不清空别人的表
	if !strings.HasPrefix(out, "#") || !strings.Contains(out, "add table inet syapp_fw\ndelete table inet syapp_fw\n") || strings.Contains(out, "flush ruleset") {
		t.Fatalf("not idempotent / touches other tables:\n%s", out)
	}
}

func TestRenderExtras(t *testing.T) {
	out, err := Render(Options{SSHFrom: []string{"10.0.0.0/8"}, SSHPort: 2222, AllowTCP: []int{9100}, AllowUDP: []int{443}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tcp dport 2222 accept", "tcp dport { 80, 443, 9100 } accept", "udp dport { 443 } accept"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "ip6 saddr") {
		t.Fatal("empty v6 set rendered")
	}
}

func TestRenderRejects(t *testing.T) {
	bad := []Options{
		{},                                      // 必须明确指定 SSH 来源
		{SSHFrom: []string{"10.0.0.0/33"}},      // 非法掩码
		{SSHFrom: []string{"10.0.0.0/8; drop"}}, // 注入
		{SSHFrom: []string{"example.com"}},
		{SSHFrom: []string{"10.0.0.1/8"}}, // 主机位非零，容易写错
		{SSHFrom: []string{"10.0.0.0/8"}, SSHPort: 0x10000},
		{SSHFrom: []string{"10.0.0.0/8"}, AllowTCP: []int{0}},
		{SSHFrom: []string{"10.0.0.0/8"}, AllowUDP: []int{70000}},
		{SSHFrom: []string{"10.0.0.0/8"}, AllowTCP: []int{80}, SSHPort: 80}, // SSH 端口不能对所有人开放
	}
	for i, o := range bad {
		if _, err := Render(o); err == nil {
			t.Errorf("case %d accepted: %+v", i, o)
		}
	}
}

func TestOpenToWorldNeedsFlag(t *testing.T) {
	if _, err := Render(Options{SSHFrom: []string{"0.0.0.0/0"}}); err == nil {
		t.Fatal("SSH open to the whole internet accepted without AllowAnySSH")
	}
	if _, err := Render(Options{SSHFrom: []string{"0.0.0.0/0"}, AllowAnySSH: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCovers(t *testing.T) {
	from := []string{"10.1.0.0/16", "2001:db8::/32", "192.0.2.7"}
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "10.2.0.1": false, "192.0.2.7": true, "2001:db8::1": true, "2001:db9::1": false,
		"::ffff:10.1.2.3": true, // IPv4 映射地址（sshd 在双栈监听时会这样报告）
	} {
		got, err := Covers(from, ip)
		if err != nil || got != want {
			t.Errorf("Covers(%s) = %v, %v; want %v", ip, got, err, want)
		}
	}
	if _, err := Covers(from, "not-an-ip"); err == nil {
		t.Fatal("bad ip accepted")
	}
}

func TestParsePorts(t *testing.T) {
	p, err := ParsePorts("9100, 8443")
	if err != nil || len(p) != 2 || p[0] != 9100 || p[1] != 8443 {
		t.Fatalf("%v %v", p, err)
	}
	if p, err := ParsePorts(""); err != nil || len(p) != 0 {
		t.Fatal("empty")
	}
	for _, s := range []string{"80;", "x", "1-100", "-1"} {
		if _, err := ParsePorts(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
