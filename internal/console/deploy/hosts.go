package deploy

import (
	"sort"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// HostNode 是主机上的一个实例（受管节点）。
type HostNode struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	GroupID        uint   `json:"group_id"`
	GroupName      string `json:"group_name"`
	Instance       string `json:"instance"` // 空串为默认实例
	Mode           string `json:"mode"`
	HTTPPort       int    `json:"http_port"`
	HTTPSPort      int    `json:"https_port"`
	Status         string `json:"status"`
	SyappVersion   string `json:"syapp_version"`
	RunningVersion string `json:"running_version"`
}

// HostView 是一台主机及其上的实例（设计 0001 第 5.2、5.6 节）。
type HostView struct {
	Host     string     `json:"host"` // HostKey 前 12 位；旧版 agent 不上报主机标识时为 node-<id>
	Name     string     `json:"name"`
	Mixed    bool       `json:"mixed"`    // 同一主机上的实例运行着不同的数据面版本
	Versions []string   `json:"versions"` // 实例实际运行的版本（去重）
	Nodes    []HostNode `json:"nodes"`
}

// Hosts 按主机列出全部受管实例（不含已吊销的节点）。
func (d *Service) Hosts() ([]HostView, error) {
	q := d.q()
	nodes, err := q.Node.WithContext(bg).Where(q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil {
		return nil, err
	}
	groups, err := q.NodeGroup.WithContext(bg).Find()
	if err != nil {
		return nil, err
	}
	gs := map[uint]*store.NodeGroup{}
	for _, g := range groups {
		gs[g.ID] = g
	}
	out := []HostView{}
	at := map[string]int{}
	for _, n := range nodes {
		k := hostKey(n)
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, HostView{Host: shortHost(k), Name: n.Hostname, Versions: []string{}})
		}
		hn := HostNode{ID: n.ID, Name: n.Name, GroupID: n.GroupID, Status: n.Status, SyappVersion: n.SyappVersion, RunningVersion: n.RunningVersion}
		if g := gs[n.GroupID]; g != nil {
			hn.GroupName = g.Name
			if p, err := groupProfile(g); err == nil {
				hn.Instance, hn.Mode, hn.HTTPPort, hn.HTTPSPort = p.Instance, p.Mode, p.HTTP(), p.HTTPS()
				if hn.Mode == "" {
					hn.Mode = "system"
				}
			}
		}
		out[i].Nodes = append(out[i].Nodes, hn)
	}
	for i := range out {
		seen := map[string]bool{}
		for _, n := range out[i].Nodes {
			if n.RunningVersion != "" && !seen[n.RunningVersion] {
				seen[n.RunningVersion] = true
				out[i].Versions = append(out[i].Versions, n.RunningVersion)
			}
		}
		sort.Strings(out[i].Versions)
		out[i].Mixed = len(out[i].Versions) > 1
	}
	return out, nil
}
