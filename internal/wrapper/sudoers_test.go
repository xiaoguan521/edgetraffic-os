package wrapper

import (
	"os"
	"strings"
	"testing"
)

// CLAUDE.md「agent 与安全」：sudoers 每个子命令单独一行（共 7 行），只有 test 与 upgrade-start 带参数通配。
// 防止再出现覆盖全部子命令的通配行（例如 `syapp-wrapper --instance *`）。
func TestSudoersContract(t *testing.T) {
	b, err := os.ReadFile("../../dataplane/packaging/sudoers/syapp-agent")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "syapp-agent ALL=(root) NOPASSWD: /usr/libexec/syapp/syapp-wrapper "
	want := []string{"test *", "reload", "reopen", "quit", "upgrade-start *", "upgrade-commit", "upgrade-abort"}
	var got []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("unexpected sudoers rule: %q", line)
		}
		got = append(got, strings.TrimPrefix(line, prefix))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sudoers rules = %q, want exactly %q", got, want)
	}
}
