package layout

import (
	"os"
	"path/filepath"
	"testing"
)

// 兼容模式（内核或 systemd 不支持 ambient 能力，如 CentOS 7）的标记：只认 root 所有、他人不可写的普通文件。
func TestRootMasterMarker(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own the marker")
	}
	fs := t.TempDir()
	if RootMasterAt(fs) {
		t.Fatal("no marker: must be the default (non-root) mode")
	}
	p := filepath.Join(fs, filepath.FromSlash(RootMasterMarker()))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RootMasterAt(fs) {
		t.Fatal("root-owned marker not recognised")
	}
	_ = os.Chmod(p, 0o666)
	if RootMasterAt(fs) {
		t.Fatal("world-writable marker accepted")
	}
	_ = os.Chmod(p, 0o644)
	_ = os.Chown(p, 65534, 65534)
	if RootMasterAt(fs) {
		t.Fatal("marker not owned by root accepted")
	}
	_ = os.Remove(p)
	_ = os.Symlink("/etc/hostname", p)
	if RootMasterAt(fs) {
		t.Fatal("symlink marker accepted")
	}
}
