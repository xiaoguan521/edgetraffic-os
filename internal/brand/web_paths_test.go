package brand

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 前端只构建一次，供任何品牌使用：界面文案里的安装路径必须取自 brand.id，不能写死默认品牌的路径，
// 否则定制品牌的控制台会显示 /opt/syedge、/etc/syedge/…（brand-matrix 第 2 步会在二进制里发现它）。
func TestWebSourceHasNoHardcodedDefaultBrandPaths(t *testing.T) {
	re := regexp.MustCompile(`/(opt|etc|data|var/lib|var/log|usr/lib/systemd/system)/syedge`)
	root := filepath.Join("..", "..", "web", "src")
	if _, err := os.Stat(root); err != nil {
		t.Skip("web/src not present")
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Ext(p) {
		case ".vue", ".ts", ".tsx", ".js":
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := re.FindString(line); m != "" {
				t.Errorf("%s:%d hard-codes %q: build the path from brand.id", p, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
