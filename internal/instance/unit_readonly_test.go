package instance

import (
	"os"
	"strings"
	"testing"
)

// 数据面单元把二进制、动态模块、版本库设为只读：worker 与 master 同一用户，被攻破的 worker 不能借此替换 master 之后会加载或执行的文件。
// 升级由单元之外的 wrapper 写入，不受影响。
func TestDataplaneUnitsMountTheCodeDirectoriesReadOnly(t *testing.T) {
	for _, name := range []string{"syedge.service", "syedge@.service"} {
		b, err := os.ReadFile("../../dataplane/packaging/systemd/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var line string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "ReadOnlyPaths=") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("%s has no ReadOnlyPaths", name)
			continue
		}
		for _, d := range []string{"-/opt/syedge/sbin", "-/opt/syedge/modules", "-/opt/syedge/versions"} {
			if !strings.Contains(line, d) {
				t.Errorf("%s: ReadOnlyPaths misses %s: %s", name, d, line)
			}
		}
	}
}

// 自定义安装根目录时，只读路径跟着改写。
func TestPrefixUnitRebasesReadOnlyPaths(t *testing.T) {
	b, err := os.ReadFile("../../dataplane/packaging/systemd/syedge.service")
	if err != nil {
		t.Fatal(err)
	}
	got := prefixUnit(string(b), "/data/syedge", true)
	if !strings.Contains(got, "ReadOnlyPaths=-/data/syedge/sbin -/data/syedge/modules -/data/syedge/versions") {
		t.Errorf("custom-root unit does not rebase ReadOnlyPaths:\n%s", got)
	}
}
