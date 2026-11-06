package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestFirewallSubcommand(t *testing.T) {
	var o, e bytes.Buffer
	if rc := firewallMain([]string{"render", "-ssh-from", "10.0.0.0/8", "-allow-udp", "443"}, &o, &e); rc != 0 || !strings.Contains(o.String(), "udp dport { 443 } accept") {
		t.Fatalf("render rc=%d %s %s", rc, o.String(), e.String())
	}
	for _, args := range [][]string{
		{"render"},
		{"render", "-ssh-from", "10.0.0.0/8", "-allow-tcp", "80;22"},
		{"render", "-ssh-from", "10.0.0.0/8\naccept"},
		{"exec", "id"},
		{},
	} {
		o.Reset()
		if rc := firewallMain(args, &o, &e); rc == 0 || o.Len() != 0 {
			t.Errorf("%q accepted (rc=%d, out=%q)", args, rc, o.String())
		}
	}
	if rc := firewallMain([]string{"covers", "-ssh-from", "10.0.0.0/8", "-ip", "10.9.9.9"}, &o, &e); rc != 0 {
		t.Fatal("covers in-range")
	}
	if rc := firewallMain([]string{"covers", "-ssh-from", "10.0.0.0/8", "-ip", "11.0.0.1"}, &o, &e); rc != 1 {
		t.Fatal("covers out-of-range")
	}
}
