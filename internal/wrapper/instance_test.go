package wrapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstance(t *testing.T, dir, body string, dirMode, fileMode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, InstanceFile), []byte(body), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, InstanceFile), fileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
}

// --instance 只能指向由 root 创建的实例：前缀目录与 instance.json 都属于 root 且不可被他人写，名称一致。
// 测试以当前用户代替 root（owner 参数），生产调用固定为 0。
func TestCheckInstanceAt(t *testing.T) {
	me := os.Getuid()
	base := t.TempDir()
	good := filepath.Join(base, "blue")
	writeInstance(t, good, `{"instance":"blue","http_port":18080,"https_port":18443,"status_port":8082}`, 0o755, 0o644)
	if err := checkInstanceAt(good, "blue", me); err != nil {
		t.Fatalf("valid instance rejected: %v", err)
	}
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkInstanceAt(good, "blue", me); err == nil {
		t.Fatal("world-writable instances parent accepted")
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, dir, inst, want string
		owner                 int
		setup                 func(dir string)
	}{
		{"missing", filepath.Join(base, "nope"), "nope", "not installed", me, nil},
		{"wrong owner", good, "blue", "owned by", me + 1, nil},
		{"name mismatch", filepath.Join(base, "green"), "green", "does not match", me, func(d string) {
			writeInstance(t, d, `{"instance":"blue"}`, 0o755, 0o644)
		}},
		{"world-writable dir", filepath.Join(base, "ww"), "ww", "writable", me, func(d string) {
			writeInstance(t, d, `{"instance":"ww"}`, 0o777, 0o644)
		}},
		{"group-writable file", filepath.Join(base, "gw"), "gw", "writable", me, func(d string) {
			writeInstance(t, d, `{"instance":"gw"}`, 0o755, 0o664)
		}},
		{"no instance.json", filepath.Join(base, "empty"), "empty", "not installed", me, func(d string) {
			writeInstance(t, d, "", 0o755, 0)
		}},
		{"user mode in system wrapper", filepath.Join(base, "um"), "um", "system", me, func(d string) {
			writeInstance(t, d, `{"instance":"um","mode":"user","root":"/home/x/edge"}`, 0o755, 0o644)
		}},
		{"bad json", filepath.Join(base, "bj"), "bj", "instance.json", me, func(d string) {
			writeInstance(t, d, `{`, 0o755, 0o644)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				c.setup(c.dir)
			}
			err := checkInstanceAt(c.dir, c.inst, c.owner)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
	// instance.json 是指向别处的软链接时拒绝（防止借软链读取或伪造）
	link := filepath.Join(base, "lnk")
	writeInstance(t, link, "", 0o755, 0)
	if err := os.Symlink(filepath.Join(good, InstanceFile), filepath.Join(link, InstanceFile)); err != nil {
		t.Fatal(err)
	}
	if err := checkInstanceAt(link, "lnk", me); err == nil {
		t.Fatal("symlinked instance.json accepted")
	}
	// 默认实例不需要 instance.json
	if err := CheckInstance(""); err != nil {
		t.Fatalf("default instance: %v", err)
	}
}

// 同机多个实例的升级状态互不覆盖；默认实例沿用 state.json。
func TestUpgradeStatePerInstance(t *testing.T) {
	dir := t.TempDir()
	def := Paths{Versions: dir}
	blue := Paths{Versions: dir, Instance: "blue"}
	if err := def.saveState(State{Target: "1.30.6", Phase: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := blue.saveState(State{Target: "1.30.7", Phase: "committed"}); err != nil {
		t.Fatal(err)
	}
	a, err := def.loadState()
	if err != nil || a.Target != "1.30.6" || a.Phase != "started" {
		t.Fatalf("default state = %+v, %v", a, err)
	}
	b, err := blue.loadState()
	if err != nil || b.Target != "1.30.7" {
		t.Fatalf("blue state = %+v, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal("default instance must keep state.json for already-deployed nodes")
	}
	if got := DefaultPathsForInstance("blue").stateName(); got != "state-blue.json" {
		t.Fatalf("blue state file = %q", got)
	}
}
