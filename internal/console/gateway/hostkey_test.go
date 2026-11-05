package gateway

import (
	"strings"
	"testing"
)

// 机器指纹只接受 sha256 十六进制：其他内容（旧版 agent 的空串、伪造的长串、大写或路径）一律视为未知主机。
func TestHostKey(t *testing.T) {
	ok := strings.Repeat("a1", 32)
	if HostKey(ok) != ok {
		t.Fatal("valid fingerprint rejected")
	}
	for _, bad := range []string{"", strings.ToUpper(ok), ok + "0", ok[:63], "../../etc/machine-id", strings.Repeat("g", 64)} {
		if HostKey(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
