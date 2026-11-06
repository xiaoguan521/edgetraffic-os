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
	// 空 /etc/machine-id 的 sha256（容器镜像中常见，所有容器相同）：不能据此认定为同一台主机
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, bad := range []string{"", strings.ToUpper(ok), ok + "0", ok[:63], "../../etc/machine-id", strings.Repeat("g", 64), empty} {
		if HostKey(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
