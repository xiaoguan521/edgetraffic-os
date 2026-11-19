package layout

import (
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// 数据面编译期的日志与缓存默认值必须与安装布局一致：否则手动启动（不带 -e）时，
// 二进制会去写 /var/log/<品牌> 而不是安装目录下的 logs。build-syedge.sh 的路径推导来自 dataplane/paths.sh。
func TestCompiledDataplanePathsMatchTheLayout(t *testing.T) {
	b := brand.ID()
	for _, root := range []string{"/opt/" + b, "/home/atwasoft/" + b, "/data/edge"} {
		out, err := exec.Command("bash", "-c", `source ../../dataplane/paths.sh && dataplane_path_flags "$1" "$2"`, "_", root, b).CombinedOutput()
		if err != nil {
			t.Fatalf("paths.sh for %s: %v\n%s", root, err, out)
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		l := SystemAt(root, "")
		want := map[string]string{
			"--error-log-path":             path.Join(l.LogDir(), "error.log"),
			"--http-log-path":              path.Join(l.LogDir(), "access.log"),
			"--http-client-body-temp-path": path.Join(l.CacheDir(), "client_temp"),
			"--http-proxy-temp-path":       path.Join(l.CacheDir(), "proxy_temp"),
			"--http-fastcgi-temp-path":     path.Join(l.CacheDir(), "fastcgi_temp"),
			"--http-uwsgi-temp-path":       path.Join(l.CacheDir(), "uwsgi_temp"),
			"--http-scgi-temp-path":        path.Join(l.CacheDir(), "scgi_temp"),
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("root %s: %s = %q, want %q (layout)", root, k, got[k], v)
			}
		}
	}
}
