package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

func TestPackageDefaults(t *testing.T) {
	root := t.TempDir()
	if err := WritePackageDefaults(root); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"share/defaults/baseline/syapp.conf", "share/defaults/baseline/" + ManifestName,
		"share/defaults/baseline/mime.types", "share/defaults/standalone-example.conf", "share/doc/STANDALONE.txt"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("missing %s", f)
		}
	}
	// 与 agent bootstrap 写入的初始版本完全一致：后装 syapp-agent 时不算漂移
	b, _ := Baseline()
	m, _ := os.ReadFile(filepath.Join(root, "share/defaults/baseline", ManifestName))
	if string(m) != string(b.Files[ManifestName]) {
		t.Fatal("packaged baseline differs from bootstrap baseline")
	}
	if !strings.Contains(string(standaloneGuide), "Based on NGINX Open Source (BSD-2-Clause)") {
		t.Fatal("guide must keep the upstream notice")
	}
}

// 示例站点必须遵守模板规则（location 内无 add_header；有 proxy_set_header 时重发基线五条）。
func TestStandaloneExampleFollowsRules(t *testing.T) {
	inLoc, depth := false, 0
	var locHeaders []string
	for _, ln := range strings.Split(string(standaloneExample), "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "#") || s == "" {
			continue
		}
		if strings.HasPrefix(s, "location ") {
			inLoc, depth, locHeaders = true, 0, nil
		}
		if inLoc {
			if strings.HasPrefix(s, "add_header") {
				t.Fatalf("add_header inside location: %s", s)
			}
			if strings.HasPrefix(s, "proxy_set_header") {
				locHeaders = append(locHeaders, strings.Fields(s)[1])
			}
			depth += strings.Count(s, "{") - strings.Count(s, "}")
			if depth == 0 && strings.Contains(s, "}") {
				if len(locHeaders) > 0 {
					got := strings.Join(locHeaders, " ")
					for _, h := range []string{"Host", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Proto", "X-Request-ID"} {
						if !strings.Contains(got, h) {
							t.Fatalf("location sets proxy headers without %s: %s", h, got)
						}
					}
				}
				inLoc = false
			}
		}
	}
}

// 基线 + 示例站点放进 conf.d 后，真实 syapp -t 必须通过。
func TestStandaloneExampleSyappSyntax(t *testing.T) {
	bin := os.Getenv("SYAPP_BIN")
	if bin == "" {
		bin = "/opt/syapp/sbin/syapp"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syapp binary not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("syapp -t needs root to check user and log paths")
	}
	b, err := Baseline()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), BaselineRevision)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, conf := range []string{"", string(standaloneExample)} {
		_ = os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755)
		if conf != "" {
			if err := os.WriteFile(filepath.Join(dir, "conf.d", "www.conf"), []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syapp.conf")).CombinedOutput(); err != nil {
			t.Fatalf("syapp -t failed (example=%v): %v\n%s", conf != "", err, out)
		}
	}
	// HTTPS 示例取消注释后（配上证书）也要能通过
	https := uncommentHTTPS(string(standaloneExample))
	if !strings.Contains(https, "listen       443  ssl;") {
		t.Fatal("https block not found")
	}
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := exec.Command("openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "1",
		"-subj", "/CN=www.example.com", "-keyout", filepath.Join(dir, "certs", "www.example.com.key"), "-out", filepath.Join(dir, "certs", "www.example.com.crt"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("openssl unavailable: %v %s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "conf.d", "www.conf"), []byte(https), 0o644)
	if out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syapp.conf")).CombinedOutput(); err != nil {
		t.Fatalf("syapp -t failed with HTTPS example: %v\n%s", err, out)
	}
}

// uncommentHTTPS 去掉示例中 HTTPS server 块的注释符号。
func uncommentHTTPS(s string) string {
	var out []string
	on := false
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "# server {") {
			on = true
		}
		if on && strings.HasPrefix(ln, "# ") {
			ln = ln[2:]
		} else if on && ln == "#" {
			ln = ""
		}
		if on && ln == "}" {
			on = false
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// 嵌入二进制的模板与随包文档不得含默认品牌名（用 @ID@ 或模板函数），否则定制品牌的二进制会残留旧名字。
func TestEmbeddedFilesHaveNoBrand(t *testing.T) {
	for _, dir := range []string{"templates", "standalone"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(b)), brand.Default) {
				t.Errorf("%s/%s contains the default brand name", dir, e.Name())
			}
		}
	}
	// 默认品牌：占位符替换后与原文一致
	if s := string(StandaloneFiles()["share/doc/STANDALONE.txt"]); strings.Contains(s, "@ID@") || !strings.Contains(s, "syapp -t") {
		t.Fatal("placeholder not expanded for the default brand")
	}
}
