// hpack-gen 输出一个小写 ASCII 字符串的 HPACK 字面量编码（RFC 7541 5.2 / 附录 B），
// 用于替换 HTTP/2 filter 中预编码的 server 响应头值。
//
//	go run ./dataplane/hpack-gen syapp
//	{ 0x84, 0x47, 0xa1, 0xd7, 0x5f }
//
// 只收录产品名可能用到的字符；新增字符时按 RFC 7541 附录 B 补表，并在测试中加入已知样例。
package main

import (
	"fmt"
	"os"
	"strings"
)

type code struct {
	bits uint32
	n    uint
}

// RFC 7541 附录 B 的子集：只收录已被测试样例覆盖的字符。
var table = map[byte]code{
	'a': {0x3, 5}, 'i': {0x6, 5}, 's': {0x8, 5},
	'g': {0x26, 6}, 'n': {0x2a, 6}, 'p': {0x2b, 6},
	'x': {0x79, 7}, 'y': {0x7a, 7},
}

// Encode 返回带长度前缀（H 位置 1）的哈夫曼编码字节。仅支持编码后不超过 126 字节的短串。
func Encode(s string) ([]byte, error) {
	var out []byte
	var acc uint64
	var nbits uint
	for i := 0; i < len(s); i++ {
		c, ok := table[s[i]]
		if !ok {
			return nil, fmt.Errorf("character %q not in table", s[i])
		}
		acc = acc<<c.n | uint64(c.bits)
		nbits += c.n
		for nbits >= 8 {
			out = append(out, byte(acc>>(nbits-8)))
			nbits -= 8
		}
	}
	if nbits > 0 { // 用 EOS 前缀（全 1）补齐
		pad := 8 - nbits
		out = append(out, byte(acc<<pad)|byte((1<<pad)-1))
	}
	if len(out) > 126 {
		return nil, fmt.Errorf("encoded length %d too long", len(out))
	}
	return append([]byte{0x80 | byte(len(out))}, out...), nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hpack-gen <string>")
		os.Exit(2)
	}
	b, err := Encode(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("0x%02x", x)
	}
	fmt.Printf("{ %s }\n", strings.Join(parts, ", "))
}
