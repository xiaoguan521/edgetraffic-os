package conn

import (
	"os"
	"path/filepath"
	"testing"
)

// /etc/machine-id 缺失或为空（容器镜像中常见）时不上报指纹：否则所有这样的机器都得到同一个指纹，被当成同一台主机。
func TestMachineFingerprintEmpty(t *testing.T) {
	d := t.TempDir()
	for _, c := range []struct{ content, want string }{
		{"", ""},
		{"  \n", ""},
		{"0123456789abcdef0123456789abcdef\n", fp([]byte("0123456789abcdef0123456789abcdef"))},
	} {
		p := filepath.Join(d, "machine-id")
		if err := os.WriteFile(p, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := fingerprintOf(p); got != c.want {
			t.Errorf("%q: got %q want %q", c.content, got, c.want)
		}
	}
	if got := fingerprintOf(filepath.Join(d, "missing")); got != "" {
		t.Errorf("missing file: %q", got)
	}
}
