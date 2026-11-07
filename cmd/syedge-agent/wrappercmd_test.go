package main

import (
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// agent 调用 wrapper 的方式：默认以数据面用户（D5）；兼容模式（master 以 root 运行，与传统 Web 服务器相同）以 root。
func TestDefaultWrapperCommand(t *testing.T) {
	sys := layout.System("")
	if got := defaultWrapper(sys, false, false); got != "sudo -n -u syedge /usr/libexec/syedge/syedge-wrapper" {
		t.Errorf("default: %q", got)
	}
	if got := defaultWrapper(sys, false, true); got != "sudo -n /usr/libexec/syedge/syedge-wrapper" {
		t.Errorf("root-master: %q", got)
	}
	if got := defaultWrapper(layout.System("blue"), false, true); got != "sudo -n /usr/libexec/syedge/syedge-wrapper --instance blue" {
		t.Errorf("root-master instance: %q", got)
	}
}
