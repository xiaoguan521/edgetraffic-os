// gen-rename-patch 由改名 patch 模板生成指定品牌的改名 patch（设计 0001 第 4.3 节）。
//
//	go run ./dataplane/gen-rename-patch -id syedge -proc SyEdge -o <输出 patch>
//
// 模板（dataplane/patches/0001-rename.patch.tmpl）中的占位符：
//
//	@ID@        品牌标识（命令名、配置变量名、内部名）     syedge
//	@IDUP@      品牌标识的大写形式（环境变量名）           SYEDGE
//	@PROC@      进程标题与对外显示名（Server 头、错误页）  SyEdge
//	@PROCLEN2@  "@PROC@: " 的字节数（日志前缀的长度参数）
//	@HPACK@     HTTP/2 server 头值的 HPACK 哈夫曼字面量编码，@HPACKLEN@ 为其字节数
//
// 生成的 patch 仍只改字符串字面量与宏的值（ADR 0001）。默认品牌生成的 patch 必须与
// dataplane/patches/0001-syapp-rename.patch 逐字节相同（TestDefaultBrandMatchesCommittedPatch）。
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/http2/hpack"
)

var (
	idRe   = regexp.MustCompile(`^[a-z][a-z0-9]{2,6}$`)
	procRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{1,14}$`)
	tokRe  = regexp.MustCompile(`@[A-Z0-9]+@`)
)

// HPACK 返回 s 的 HPACK 哈夫曼字面量编码（RFC 7541 5.2）：首字节为 H 位加长度，随后为编码数据。
func HPACK(s string) []byte {
	enc := hpack.AppendHuffmanString(nil, s)
	return append([]byte{0x80 | byte(len(enc))}, enc...)
}

func hexList(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("0x%02x", x)
	}
	return strings.Join(parts, ", ")
}

// Generate 按品牌替换模板中的占位符；有未知占位符或品牌名不合法时返回错误。
func Generate(tmpl []byte, id, proc string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("brand id %q must match %s", id, idRe)
	}
	if proc == "" {
		proc = id
	}
	if !procRe.MatchString(proc) {
		return nil, fmt.Errorf("process name %q must match %s", proc, procRe)
	}
	h := HPACK(proc)
	if len(h) > 127 {
		return nil, fmt.Errorf("process name %q too long for a one-byte HPACK length", proc)
	}
	vals := map[string]string{
		"@ID@":       id,
		"@IDUP@":     strings.ToUpper(id),
		"@PROC@":     proc,
		"@PROCLEN2@": strconv.Itoa(len(proc) + 2),
		"@HPACK@":    hexList(h),
		"@HPACKLEN@": strconv.Itoa(len(h)),
	}
	var bad []string
	out := tokRe.ReplaceAllFunc(tmpl, func(t []byte) []byte {
		v, ok := vals[string(t)]
		if !ok {
			bad = append(bad, string(t))
			return t
		}
		return []byte(v)
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("unknown placeholders in template: %v", bad)
	}
	return foldNoops(out), nil
}

// foldNoops 把代入后内容相同的相邻 "-行"/"+行" 还原为上下文行（例如默认品牌下长度常量不变）。
// 两侧行数都不变，hunk 头仍然正确；默认品牌生成的 patch 因此与已提交的 patch 逐字节相同。
func foldNoops(p []byte) []byte {
	lines := strings.Split(string(p), "\n")
	for i := 0; i+1 < len(lines); i++ {
		a, b := lines[i], lines[i+1]
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "---") && strings.HasPrefix(b, "+") && !strings.HasPrefix(b, "+++") && a[1:] == b[1:] {
			lines[i] = " " + a[1:]
			lines = append(lines[:i+1], lines[i+2:]...)
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func main() {
	id := flag.String("id", "syapp", "品牌标识（小写，3–7 个字符）")
	proc := flag.String("proc", "", "进程标题与对外显示名（默认同 -id）")
	tmplPath := flag.String("tmpl", "dataplane/patches/0001-rename.patch.tmpl", "模板")
	outPath := flag.String("o", "", "输出 patch（默认输出到标准输出）")
	flag.Parse()
	tmpl, err := os.ReadFile(*tmplPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := Generate(tmpl, *id, *proc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *outPath == "" {
		_, _ = os.Stdout.Write(out)
		return
	}
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
