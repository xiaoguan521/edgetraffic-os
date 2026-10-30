// Package ipsets 解析地域库数据（P10 规则积木的 IP 集）。
//
// 地域数据来自五大 RIR 公开的分配统计文件（delegated-<rir>-extended-latest，格式见 RIR 统计交换格式）：
// 每行 registry|cc|type|start|value|date|status[...]，ipv4 的 value 是地址数，ipv6 的 value 是前缀长度。
// 控制台按国家 / 地区代码挑出 allocated / assigned 的记录转成网段，存为节点组的 IP 集。
package ipsets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var ccRe = regexp.MustCompile(`^[A-Z]{2}$`)

// ParseRIR 从 RIR 分配统计文件中取出某国家 / 地区（ISO 3166 两位代码）的网段。
func ParseRIR(r io.Reader, cc string, max int) ([]string, error) {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if !ccRe.MatchString(cc) {
		return nil, errors.New("国家 / 地区代码必须是两位字母，例如 CN、HK")
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	var out []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 7 || strings.ToUpper(f[1]) != cc || (f[6] != "allocated" && f[6] != "assigned") {
			continue
		}
		switch f[2] {
		case "ipv4":
			start, err := netip.ParseAddr(f[3])
			n, err2 := strconv.ParseUint(f[4], 10, 32)
			if err != nil || err2 != nil || !start.Is4() || n == 0 {
				continue
			}
			ps, err := rangeToCIDRs(start, uint32(n))
			if err != nil {
				continue
			}
			out = append(out, ps...)
		case "ipv6":
			a, err := netip.ParseAddr(f[3])
			l, err2 := strconv.Atoi(f[4])
			if err != nil || err2 != nil || !a.Is6() || l < 8 || l > 128 {
				continue
			}
			out = append(out, netip.PrefixFrom(a, l).Masked().String())
		}
		if len(out) > max {
			return nil, fmt.Errorf("%s 的网段超过 %d 条", cc, max)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("文件中没有 %s 的分配记录（需要 RIR 的 delegated 统计文件）", cc)
	}
	return out, nil
}

// rangeToCIDRs 把 [start, start+n) 拆成最少的对齐网段。
func rangeToCIDRs(start netip.Addr, n uint32) ([]string, error) {
	b := start.As4()
	cur := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	end := cur + uint64(n)
	if end > 1<<32 {
		return nil, errors.New("range overflows")
	}
	var out []string
	for cur < end {
		size := uint64(1) << uint(bits.TrailingZeros32(uint32(cur)))
		if cur == 0 {
			size = 1 << 32
		}
		for size > end-cur {
			size >>= 1
		}
		plen := 32 - bits.TrailingZeros64(size)
		a := netip.AddrFrom4([4]byte{byte(cur >> 24), byte(cur >> 16), byte(cur >> 8), byte(cur)})
		out = append(out, netip.PrefixFrom(a, plen).String())
		cur += size
	}
	return out, nil
}
