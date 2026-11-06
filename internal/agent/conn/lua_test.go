package conn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLuaAddonDetection(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{C: Config{Modules: dir}}
	if a.luaAddon() != "" {
		t.Fatal("reported an add-on that is not installed")
	}
	_ = os.MkdirAll(filepath.Join(dir, "lua-1.30.6-2"), 0o755)
	_ = os.Symlink("lua-1.30.6-2", filepath.Join(dir, "lua"))
	_ = os.WriteFile(filepath.Join(dir, "lua-1.30.6-2/VERSION"), []byte(`{"name":"syedge-lua","version":"1.30.6-2"}`), 0o644)
	if a.luaAddon() != "" {
		t.Fatal("incomplete install (no BASE) reported")
	}
	_ = os.WriteFile(filepath.Join(dir, "lua-1.30.6-2/BASE"), []byte("gmssl\n"), 0o644)
	if got := a.luaAddon(); got != "1.30.6-2/gmssl" {
		t.Fatalf("luaAddon = %q", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "lua-1.30.6-2/VERSION"), []byte(`{"version":"1.30.6; rm -rf /"}`), 0o644)
	if a.luaAddon() != "" {
		t.Fatal("bad version reported")
	}
}
