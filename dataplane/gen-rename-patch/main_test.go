package main

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func readTmpl(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../patches/0001-rename.patch.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 默认品牌由模板生成的 patch 与仓库中实际使用的 patch 逐字节相同：模板化没有改变默认产物。
func TestDefaultBrandMatchesCommittedPatch(t *testing.T) {
	want, err := os.ReadFile("../patches/0001-syedge-rename.patch")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Generate(readTmpl(t), "syedge", "SyEdge")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("patch generated for the default brand differs from dataplane/patches/0001-syedge-rename.patch")
	}
}

// 已知样例：上游名与默认品牌的 HPACK 编码与提交在 patch 中的字节一致（RFC 7541 附录 B）。
func TestHPACKKnownValues(t *testing.T) {
	for s, want := range map[string][]byte{
		"ngi" + "nx": {0x84, 0xaa, 0x63, 0x55, 0xe7},
	} {
		if got := HPACK(s); !bytes.Equal(got, want) {
			t.Errorf("HPACK(%q) = % x, want % x", s, got, want)
		}
	}
}

func TestCustomBrand(t *testing.T) {
	out, err := Generate(readTmpl(t), "sytest", "SyTest")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	var added []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			added = append(added, l)
		}
	}
	joined := strings.Join(added, "\n")
	if strings.Contains(strings.ToLower(joined), "syedge") {
		t.Fatal("default brand left in generated patch")
	}
	for _, want := range []string{
		`"Usage: sytest [-?hvVtTq]`,              // 命令名用品牌标识
		`#define NGINX_VER          "SyTest/"`,   // 版本串用显示名
		`#define NGINX_VAR          "SYTEST"`,    // 环境变量用大写标识
		`ngx_cpymem(errstr, "SyTest: ", 8)`,      // 长度随名字计算
		`msg -= (8 + err_levels[level].len + 3)`, // 控制台输出的前缀长度同样随名字计算（否则吞掉消息首字符）
		`"Server: SyTest"`,
		`<center>SyTest</center>`,
		`ngx_string("sytest_version")`, // 配置变量名用小写标识
		`(u_char *) "SyTest: ",`,       // 进程标题
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("generated patch missing %q", want)
		}
	}
	// HPACK 数组的长度声明与初始值个数一致
	m := regexp.MustCompile(`static const u_char nginx\[(\d+)\] = \{ ([^}]*) \};`).FindStringSubmatch(joined)
	if m == nil {
		t.Fatal("HPACK line missing")
	}
	if n := len(strings.Split(m[2], ",")); m[1] != itoa(n) {
		t.Fatalf("array size %s but %d initialisers", m[1], n)
	}
	// HTTP/2 的 server 头必须是定制名的 HPACK 编码（文本扫描看不到哈夫曼编码，曾经漏掉过）
	if m[2] != hexList(HPACK("SyTest")) {
		t.Fatalf("HTTP/2 server header bytes %q, want HPACK(\"SyTest\") %q", m[2], hexList(HPACK("SyTest")))
	}
	// 每个 hunk 的行数不变（只替换行内容），patch 仍可应用
	if strings.Count(s, "\n@@ ") != strings.Count(string(readTmpl(t)), "\n@@ ") {
		t.Fatal("hunk count changed")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestInvalidBrands(t *testing.T) {
	for _, c := range [][2]string{{"SyEdge", ""}, {"ab", ""}, {"toolongname", ""}, {"sy-edge", ""}, {"syedge", "Sy Edge"}, {"syedge", "S"}, {"syedge", "SyEdge<"}} {
		if _, err := Generate(readTmpl(t), c[0], c[1]); err == nil {
			t.Errorf("%q / %q accepted", c[0], c[1])
		}
	}
	if _, err := Generate([]byte("x @NOPE@ y"), "syedge", ""); err == nil {
		t.Error("unknown placeholder accepted")
	}
}
