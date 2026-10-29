// Package firewall 生成边缘节点可选的 nftables 入站防火墙（规格书 P6-6）。
//
// 只由管理员在节点上显式启用（syapp-firewall enable），agent 守护进程与控制台协议都不涉及防火墙。
// 规则只放在自己的表 inet syapp_fw 中，只过滤入站：
//   - 放行已建立连接、回环、ICMP/ICMPv6（PMTU 与邻居发现必需）
//   - 所有来源可访问 HTTP/HTTPS（以及管理员追加的端口）
//   - SSH 只对明确指定的来源开放；其余入站丢弃
//
// 出站不限制：agent 需要主动连接控制台。
package firewall

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Table 是规则所在的 nftables 表名。
const Table = "syapp_fw"

// Options 是防火墙参数。
type Options struct {
	SSHFrom     []string // 允许 SSH 的来源（CIDR 或单个 IP），必填
	SSHPort     int      // 默认 22
	AllowTCP    []int    // 在 80、443 之外对所有来源开放的 TCP 端口
	AllowUDP    []int    // 对所有来源开放的 UDP 端口（如 HTTP/3 的 443）
	AllowAnySSH bool     // 允许 SSH 来源为 0.0.0.0/0 或 ::/0（不建议）
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("无效的地址段 %q", s)
		}
		if p.Masked() != p {
			return netip.Prefix{}, fmt.Errorf("地址段 %q 的主机位不为 0，是否想写 %s？", s, p.Masked())
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("无效的地址 %q", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func validPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("无效端口 %d", p)
	}
	return nil
}

// ParsePorts 解析逗号分隔的端口列表。
func ParsePorts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || validPort(n) != nil {
			return nil, fmt.Errorf("无效端口 %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}

func portSet(ports []int) string {
	seen := map[int]bool{}
	var ps []int
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			ps = append(ps, p)
		}
	}
	sort.Ints(ps)
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return "{ " + strings.Join(s, ", ") + " }"
}

// Render 校验参数并生成 nft -f 可直接加载的规则文件。
func Render(o Options) (string, error) {
	if len(o.SSHFrom) == 0 {
		return "", errors.New("必须用 --ssh-from 指定允许 SSH 的来源，避免把自己锁在外面")
	}
	if o.SSHPort == 0 {
		o.SSHPort = 22
	}
	if err := validPort(o.SSHPort); err != nil {
		return "", err
	}
	var v4, v6 []string
	for _, s := range o.SSHFrom {
		p, err := parsePrefix(s)
		if err != nil {
			return "", err
		}
		if p.Bits() == 0 && !o.AllowAnySSH {
			return "", fmt.Errorf("SSH 来源 %s 等于对整个互联网开放；确需如此请加 --allow-any-ssh", p)
		}
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	tcp := []int{80, 443}
	for _, p := range o.AllowTCP {
		if err := validPort(p); err != nil {
			return "", err
		}
		if p == o.SSHPort {
			return "", fmt.Errorf("端口 %d 是 SSH 端口，不能对所有来源开放", p)
		}
		tcp = append(tcp, p)
	}
	for _, p := range o.AllowUDP {
		if err := validPort(p); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	b.WriteString("# 由 syapp-firewall 生成，请勿手工修改；重新生成：syapp-firewall enable ...\n")
	// add + delete：表不存在时也不报错，重新加载时整表替换（原子），不影响其他表
	fmt.Fprintf(&b, "add table inet %s\ndelete table inet %s\n\n", Table, Table)
	fmt.Fprintf(&b, "table inet %s {\n", Table)
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tct state invalid drop\n")
	b.WriteString("\t\tiif \"lo\" accept\n")
	b.WriteString("\t\tip protocol icmp icmp type { echo-request, destination-unreachable, time-exceeded, parameter-problem } limit rate 20/second accept\n")
	b.WriteString("\t\tip6 nexthdr icmpv6 icmpv6 type { echo-request, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	fmt.Fprintf(&b, "\t\ttcp dport %s accept\n", portSet(tcp))
	if len(o.AllowUDP) > 0 {
		fmt.Fprintf(&b, "\t\tudp dport %s accept\n", portSet(o.AllowUDP))
	}
	if len(v4) > 0 {
		fmt.Fprintf(&b, "\t\tip saddr { %s } tcp dport %d accept\n", strings.Join(v4, ", "), o.SSHPort)
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "\t\tip6 saddr { %s } tcp dport %d accept\n", strings.Join(v6, ", "), o.SSHPort)
	}
	b.WriteString("\t\tcounter\n")
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

// Covers 判断 ip 是否在 from 列表中（用于启用前检查当前 SSH 会话不会被断开）。
func Covers(from []string, ip string) (bool, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false, fmt.Errorf("无效的地址 %q", ip)
	}
	a = a.Unmap()
	for _, s := range from {
		p, err := parsePrefix(s)
		if err != nil {
			return false, err
		}
		if p.Contains(a) {
			return true, nil
		}
	}
	return false, nil
}
