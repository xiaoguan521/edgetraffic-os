package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func instanceProfile() Profile {
	return Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}
}

func TestHistoricalProfileDoesNotFollowCompiledDefault(t *testing.T) {
	l, err := (Profile{}).Layout()
	if err != nil || l.Root() != "/opt/syedge" {
		t.Fatalf("historical empty profile root = %q, %v", l.Root(), err)
	}
	if !(Profile{}).SameInstance(Profile{Mode: "system", Root: "/opt/syedge"}) {
		t.Fatal("historical and explicit legacy profiles differ")
	}
	if layout.DefaultSystemRoot() != "/opt/syedge" && (Profile{}).SameInstance(Profile{Mode: "system", Root: layout.DefaultSystemRoot()}) {
		t.Fatal("new compiled root was mistaken for a legacy profile")
	}
}

// 具名实例：所有路径与端口都来自档案，渲染结果中不残留默认实例的路径或端口（设计 0001 第 3、5.3 节）。
func TestRenderInstanceProfile(t *testing.T) {
	in := sampleInput(t)
	in.Profile = instanceProfile()
	in.Sites[1].Routes[0].Static.Files = map[string][]byte{"index.html": []byte("x")}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	for _, want := range []string{
		"pid   /opt/syedge/instances/blue/run/syedge.pid;",
		"error_log  /opt/syedge/instances/blue/logs/error.log  warn;",
		"access_log  /opt/syedge/instances/blue/logs/access.json  edge_json;",
		"client_body_temp_path  /opt/syedge/instances/blue/cache/client_temp;",
		"proxy_temp_path        /opt/syedge/instances/blue/cache/proxy_temp;",
		"listen       18080  default_server;",
		"listen       18443  ssl  default_server;",
		"listen       127.0.0.1:8082;",
		"location = /__syedge_status {",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main config missing %q", want)
		}
	}
	all := ""
	for n, c := range b.Files {
		if strings.HasSuffix(n, ".conf") {
			all += string(c)
		}
	}
	for _, bad := range []string{"pid   /run/", " /var/log/syedge", " /var/cache/syedge", "/opt/syedge/conf/", " 80;", " 80 ", " 443 ", " 443;", ":8081"} {
		if strings.Contains(all, bad) {
			t.Errorf("instance render still contains default %q", bad)
		}
	}
	site := string(b.Files["conf.d/site-blog.conf"])
	if !strings.Contains(site, "listen       18080;") || !strings.Contains(site, "alias  /opt/syedge/instances/blue/conf/current/sites/blog/") {
		t.Errorf("site config not using profile:\n%s", site)
	}
	if !strings.Contains(string(b.Files["conf.d/site-shop.conf"]), "listen       18443  ssl;") {
		t.Error("https site not using profile port")
	}
}

// user 模式：没有 user 指令，全部路径在安装根目录下，Lua 模块用绝对路径，文件数上限取自档案。
func TestRenderUserProfile(t *testing.T) {
	in := luaInput(t)
	in.Profile = Profile{Mode: "user", Root: "/home/ops/edge", HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8082, NoFile: 4096}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	if strings.Contains(main, "\nuser ") {
		t.Error("user mode must not emit the user directive")
	}
	for _, want := range []string{
		"worker_rlimit_nofile  4096;",
		"pid   /home/ops/edge/run/syedge.pid;",
		"error_log  /home/ops/edge/logs/error.log  warn;",
		"client_body_temp_path  /home/ops/edge/cache/client_temp;",
		"load_module  /home/ops/edge/modules/lua/ndk_http_module.so;",
		`lua_package_path   "/home/ops/edge/modules/lua/lualib/?.lua";`,
		"/home/ops/edge/conf/releases/" + in.Revision + "/lua/",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("user-mode main config missing %q", want)
		}
	}
}

func TestRenderCustomSystemProfile(t *testing.T) {
	in := luaInput(t)
	in.Profile = Profile{Mode: "system", Root: "/data/syedge"}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	for _, want := range []string{
		"pid   /data/syedge/run/syedge.pid;",
		"error_log  /data/syedge/logs/error.log  warn;",
		"client_body_temp_path  /data/syedge/cache/client_temp;",
		"load_module  /data/syedge/modules/lua/ndk_http_module.so;",
		`lua_package_path   "/data/syedge/modules/lua/lualib/?.lua";`,
		"/data/syedge/conf/releases/" + in.Revision + "/lua/",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("custom system main config missing %q", want)
		}
	}
	if strings.Contains(main, "user ") {
		t.Error("custom system mode must retain the system user directive")
	}
}

func TestRenderIndependentInstanceLuaPaths(t *testing.T) {
	in := luaInput(t)
	in.Profile = Profile{Instance: "blue", ResourceMode: "independent", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	for _, want := range []string{
		"load_module  /opt/syedge/instances/blue/modules/lua/ndk_http_module.so;",
		`lua_package_path   "/opt/syedge/instances/blue/modules/lua/lualib/?.lua";`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if in.Profile.SameInstance(Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}) {
		t.Fatal("independent and shared instances must not compare equal")
	}
	if err := (Profile{ResourceMode: "independent"}).Validate(); err == nil {
		t.Fatal("default instance cannot be assigned an independent runtime")
	}
	if err := (Profile{Instance: "blue", ResourceMode: "unknown"}).Validate(); err == nil {
		t.Fatal("unknown runtime mode accepted")
	}
}

func TestProfileValidation(t *testing.T) {
	ok := []Profile{{}, instanceProfile(), {Root: "/data/edge"}, {Mode: "system", Root: "/data/edge"}, {Mode: "system", Root: "/home/atwasoft/node"}, {Mode: "user", Root: "/data/edge", HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8082}}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", p, err)
		}
	}
	bad := []Profile{
		{Instance: "Bad"},
		{Instance: "default"},
		{Mode: "weird"},
		{Mode: "system", Root: "/usr"},
		{Mode: "system", Root: "/home"},
		{Mode: "user"},                     // 缺少根目录
		{Mode: "user", Root: "relative"},   // 根目录必须是绝对路径
		{Mode: "user", Root: "/data/edge"}, // user 模式不能用 80/443（默认端口）
		{HTTPPort: 8080, HTTPSPort: 8080},  // 端口重复
		{StatusPort: 443},                  // 与 HTTPS 端口冲突
		{HTTPPort: 70000},                  // 越界
		{NoFile: -1},                       // 越界
		{Mode: "user", Root: "/data/edge", HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 22}, // 保留端口
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	// 四层代理不能占用档案中的端口
	in := sampleInput(t)
	in.Profile = instanceProfile()
	in.Streams = []StreamProxy{{Name: "mysql", Listen: 18080, Servers: []UpstreamServer{{Addr: "10.0.0.1:1"}}}}
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "port 18080 not allowed") {
		t.Errorf("stream on the instance HTTP port: want port rejection, got %v", err)
	}
	in.Streams[0].Listen = 80 // 默认实例的端口对具名实例并不保留
	if _, err := Render(in); err != nil {
		t.Errorf("stream on 80 rejected for a non-default instance: %v", err)
	}
}

// 证书续期重打包时，只改写本实例版本目录下的绝对路径。
func TestRebaseRevisionUsesProfile(t *testing.T) {
	p := instanceProfile()
	src := map[string][]byte{"a.conf": []byte(p.ReleasesDir() + "r000001/certs/x /opt/syedge/conf/releases/r000001/y")}
	out := RebaseRevisionFor(p, src, "r000001", "r000002")
	want := p.ReleasesDir() + "r000002/certs/x /opt/syedge/conf/releases/r000001/y"
	if got := string(out["a.conf"]); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 具名实例与 user 模式的渲染结果也必须通过真实 syedge -t（CLAUDE.md「配置」：修改模板须过真实 syedge -t）。
// 在 ci 工作流的 dataplane 任务中以 root 运行；没有二进制或非 root 时跳过。
func TestRenderProfileSyedgeSyntax(t *testing.T) {
	bin := os.Getenv("SYEDGE_BIN")
	if bin == "" {
		bin = filepath.Join(layout.DefaultSystemRoot(), "sbin", "syedge")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syedge binary not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("syedge -t needs root to check user and log paths")
	}
	userRoot := filepath.Join(t.TempDir(), "edge")
	for name, p := range map[string]Profile{
		"instance": instanceProfile(),
		"user":     {Mode: "user", Root: userRoot, HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8082, NoFile: 4096},
	} {
		t.Run(name, func(t *testing.T) {
			in := sampleInput(t)
			in.Profile = p
			if mt, err := os.ReadFile(filepath.Join(layout.DefaultSystemRoot(), "share/defaults/mime.types")); err == nil {
				in.MimeTypes = mt
			}
			in.Sites[1].Routes[0].Static.Files = map[string][]byte{"index.html": []byte("<h1>x</h1>")}
			in.Streams = []StreamProxy{{Name: "mysql", Listen: 13306, Servers: []UpstreamServer{{Addr: "10.0.1.21:3306"}}}}
			b, err := Render(in)
			if err != nil {
				t.Fatal(err)
			}
			l, _ := p.Layout()
			// 安装程序负责创建的实例目录（都在实例前缀下）
			for _, d := range []string{l.LogDir(), l.CacheDir(), l.RunDir()} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if l.Mode() == "system" {
					d := d
					t.Cleanup(func() { _ = os.RemoveAll(d) })
				}
			}
			dir := filepath.Join(t.TempDir(), in.Revision)
			if err := b.WriteDir(dir); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syedge.conf")).CombinedOutput()
			if err != nil {
				t.Fatalf("syedge -t failed: %v\n%s", err, out)
			}
		})
	}
}

// D5：system 模式的 master 也不以 root 运行，worker 与 master 同一用户，配置中不再有 user 指令
// （非 root 的 master 会忽略它并在启动时告警）。
func TestNoUserDirective(t *testing.T) {
	for _, p := range []Profile{{}, instanceProfile()} {
		in := sampleInput(t)
		in.Profile = p
		b, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		if main := string(b.Files["syedge.conf"]); strings.Contains(main, "\nuser ") {
			t.Fatalf("profile %+v emits a user directive", p)
		}
	}
}
