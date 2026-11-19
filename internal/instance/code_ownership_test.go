package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func chowns(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "chown ") {
			out = append(out, c)
		}
	}
	return out
}

func hasChown(cmds []string, want string) bool {
	for _, c := range chowns(cmds) {
		if c == want {
			return true
		}
	}
	return false
}

// 兼容模式的 master 以 root 运行，会加载 modules/ 下的模块、执行 versions/ 下的二进制。
// 这些目录若仍属于数据面用户（此前处于普通模式时设置的），被攻破的 worker 能替换文件并在下次 reload 时以 root 执行。
func TestCompatModeTakesTheCodeDirectoriesBackToRoot(t *testing.T) {
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	f.Ambient = func() bool { return true }
	f.Handover = func(string) error { return nil }
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if !hasChown(f.cmds, "chown syedge:syedge /opt/syedge/sbin") {
		t.Fatalf("precondition: normal mode did not give sbin to the data-plane user: %v", chowns(f.cmds))
	}
	n := len(f.cmds)
	f.RootMaster = true
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	after := f.cmds[n:]
	for _, want := range []string{
		"chown root:root /opt/syedge/sbin",
		"chown root:root /opt/syedge/modules",
		"chown -R root:root /opt/syedge/versions",
	} {
		if !hasChown(after, want) {
			t.Errorf("compat mode did not run %q; chown commands: %v", want, chowns(after))
		}
	}
	for _, c := range chowns(after) {
		if strings.Contains(c, "syedge:syedge /opt/syedge/sbin") || strings.Contains(c, "syedge:syedge /opt/syedge/modules") || strings.Contains(c, "syedge:syedge /opt/syedge/versions") {
			t.Errorf("compat mode still hands code directories to the data-plane user: %q", c)
		}
	}
}

// 回到普通模式后，目录再交给数据面用户（wrapper 以该用户运行，需要写入版本库）。
func TestLeavingCompatModeGivesTheCodeDirectoriesToTheDataPlaneUserAgain(t *testing.T) {
	f := newFake(t)
	_ = os.MkdirAll(f.path(layout.System("").ConfRoot()), 0o755)
	f.Ambient = func() bool { return true }
	f.Handover = func(string) error { return nil }
	f.RootMaster = true
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(f.path(layout.RootMasterMarker()))
	f.RootMaster = false
	n := len(f.cmds)
	if err := f.Nonroot(); err != nil {
		t.Fatal(err)
	}
	if !hasChown(f.cmds[n:], "chown syedge:syedge /opt/syedge/sbin") {
		t.Fatalf("leaving compat mode did not hand sbin back: %v", chowns(f.cmds[n:]))
	}
}
