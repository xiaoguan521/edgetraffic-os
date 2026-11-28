package collect

import (
	"net"
	"net/netip"
	"sort"
)

const maxLocalIPs = 64

// LocalIPs 只读采集本机网卡当前绑定的非回环、非链路本地单播 IP（用于识别 Keepalived VIP 归属，无需 root 权限）。
func LocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !addr.IsValid() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
			continue
		}
		s := addr.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > maxLocalIPs {
		out = out[:maxLocalIPs]
	}
	return out
}
