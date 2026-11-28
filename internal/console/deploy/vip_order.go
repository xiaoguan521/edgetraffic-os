package deploy

import (
	"net/netip"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// parseGroupVIPs 解析节点组登记的入口 VIP 集合（仅保留合法 IP）。
func parseGroupVIPs(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if addr, err := netip.ParseAddr(s); err == nil {
			out[addr.Unmap().String()] = true
		}
	}
	return out
}

// nodeHoldsVIP 判断节点当前上报的网卡 IP 是否包含任一目标 VIP。
func nodeHoldsVIP(n *store.Node, vips map[string]bool) bool {
	if n == nil || len(vips) == 0 || n.LocalIPs == "" {
		return false
	}
	for _, s := range strings.Split(n.LocalIPs, ",") {
		if vips[strings.TrimSpace(s)] {
			return true
		}
	}
	return false
}

// orderBackupFirst 将未持有 VIP 的节点（备机）排在前面，持有 VIP 的节点（主机）排在最后，保持原有相对顺序。
func orderBackupFirst(nodes []*store.Node, vips map[string]bool) []*store.Node {
	if len(vips) == 0 || len(nodes) <= 1 {
		return nodes
	}
	out := append([]*store.Node(nil), nodes...)
	sort.SliceStable(out, func(i, j int) bool {
		hi := nodeHoldsVIP(out[i], vips)
		hj := nodeHoldsVIP(out[j], vips)
		if hi != hj {
			return !hi // 备机在前、主机在后
		}
		return false
	})
	return out
}

// groupVIPSet 查询涉及节点组登记的全部入口 VIP 集合（用于同机多实例升级时统一避让任意承载 VIP 的主机）。
func (d *Service) groupVIPSet(groupIDs ...uint) map[string]bool {
	out := map[string]bool{}
	seen := map[uint]bool{}
	for _, gid := range groupIDs {
		if gid == 0 || seen[gid] {
			continue
		}
		seen[gid] = true
		g, err := store.First[store.NodeGroup](d.Store, gid)
		if err != nil || g.VIPs == "" {
			continue
		}
		for vip := range parseGroupVIPs(g.VIPs) {
			out[vip] = true
		}
	}
	return out
}
