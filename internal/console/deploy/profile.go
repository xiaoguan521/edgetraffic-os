package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 节点组实例档案（设计 0001 第 3.4、5.1、5.2 节）。
//
// 同组节点共用一个档案，因此同组节点拿到逐字节相同的配置包。同一台主机上的多个实例必然属于不同的组，
// 主机以 Node.HostKey（/etc/machine-id 的 sha256）识别；同一主机上的实例名不能相同，端口（含四层代理）不能重叠。

var (
	// ErrProfileLocked 表示组内已有节点，实例名、模式、根目录不能再改（节点上的目录已按它们创建）。
	ErrProfileLocked = errors.New("PROFILE_LOCKED: 节点组内已有节点，实例名、安装模式与安装根目录不能修改；如需迁移请新建节点组")
	// ErrHostConflict 表示与同一主机上的其他实例冲突。
	ErrHostConflict = errors.New("HOST_CONFLICT")
)

// groupProfile 解析节点组的档案；空串为默认实例。
func groupProfile(g *store.NodeGroup) (render.Profile, error) {
	var p render.Profile
	if strings.TrimSpace(g.InstanceProfile) == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(g.InstanceProfile), &p); err != nil {
		return p, fmt.Errorf("node group %s: instance profile: %w", g.Name, err)
	}
	return p, nil
}

// Profile 返回节点组的实例档案。
func (d *Service) Profile(groupID uint) (render.Profile, error) {
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return render.Profile{}, err
	}
	return groupProfile(g)
}

// SetProfile 校验并保存节点组的实例档案。
func (d *Service) SetProfile(groupID uint, p render.Profile, user, ip string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	lk := d.groupLock(groupID)
	lk.Lock()
	defer lk.Unlock()
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return err
	}
	old, err := groupProfile(g)
	if err != nil {
		return err
	}
	q := d.q()
	if n, err := q.Node.WithContext(bg).Where(q.Node.GroupID.Eq(groupID)).Count(); err != nil {
		return err
	} else if n > 0 && (old.Instance != p.Instance || old.Mode != p.Mode || old.Root != p.Root) {
		return ErrProfileLocked
	}
	streams, err := d.groupStreams(groupID)
	if err != nil {
		return err
	}
	if err := d.hostConflicts(groupID, p, streams, d.groupHostKeys(groupID), nil); err != nil {
		return err
	}
	b, _ := json.Marshal(p)
	if p == (render.Profile{}) {
		b = nil // 默认实例存空串，与引入档案前一致
	}
	if _, err := q.NodeGroup.WithContext(bg).Where(q.NodeGroup.ID.Eq(groupID)).Update(q.NodeGroup.InstanceProfile, string(b)); err != nil {
		return err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "group.profile", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]any{"old": old, "new": p}})
	return nil
}

// CheckStreamPorts 检查保存四层代理后，本组端口是否与同一主机上的其他实例冲突（同组内的冲突由渲染器检查）。
func (d *Service) CheckStreamPorts(groupID uint, sp render.StreamProxy) error {
	p, err := d.Profile(groupID)
	if err != nil {
		return err
	}
	streams, err := d.groupStreams(groupID)
	if err != nil {
		return err
	}
	keep := streams[:0]
	for _, s := range streams {
		if s.Name != sp.Name {
			keep = append(keep, s)
		}
	}
	return d.hostConflicts(groupID, p, append(keep, sp), d.groupHostKeys(groupID), nil)
}

// CheckEnrollHost 在节点注册前检查：该主机上是否已有与本组档案冲突的实例。
// selfNodeID 为重新注册的节点自身（不与自己比较）；hostKey 为空（旧版 agent 不上报）时无法检查，放行。
func (d *Service) CheckEnrollHost(groupID uint, hostKey string, selfNodeID uint) error {
	if hostKey == "" {
		return nil
	}
	p, err := d.Profile(groupID)
	if err != nil {
		return err
	}
	streams, err := d.groupStreams(groupID)
	if err != nil {
		return err
	}
	return d.hostConflicts(groupID, p, streams, []string{hostKey}, &selfNodeID)
}

// groupHostKeys 返回组内节点所在的主机。
func (d *Service) groupHostKeys(groupID uint) []string {
	q := d.q()
	nodes, _ := q.Node.WithContext(bg).Select(q.Node.HostKey).Where(q.Node.GroupID.Eq(groupID), q.Node.HostKey.Neq("")).Find()
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if !seen[n.HostKey] {
			seen[n.HostKey] = true
			out = append(out, n.HostKey)
		}
	}
	return out
}

// groupStreams 返回组内已启用的四层代理。
func (d *Service) groupStreams(groupID uint) ([]render.StreamProxy, error) {
	q := d.q()
	rows, err := q.StreamService.WithContext(bg).Where(q.StreamService.GroupID.Eq(groupID), q.StreamService.Enabled.Is(true)).Find()
	if err != nil {
		return nil, err
	}
	out := make([]render.StreamProxy, 0, len(rows))
	for _, r := range rows {
		var sp render.StreamProxy
		if err := json.Unmarshal([]byte(r.Spec), &sp); err != nil {
			return nil, fmt.Errorf("stream %s: %w", r.Name, err)
		}
		sp.Name = r.Name
		out = append(out, sp)
	}
	return out, nil
}

// hostPorts 返回一个实例在主机上占用的端口（"tcp/80"、"udp/53" 形式 → 用途）。
func hostPorts(p render.Profile, streams []render.StreamProxy) map[string]string {
	out := map[string]string{
		fmt.Sprintf("tcp/%d", p.HTTP()):   "HTTP",
		fmt.Sprintf("tcp/%d", p.HTTPS()):  "HTTPS",
		fmt.Sprintf("tcp/%d", p.Status()): "状态页",
	}
	for _, s := range streams {
		proto := s.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out[fmt.Sprintf("%s/%d", proto, s.Listen)] = "四层代理 " + s.Name
	}
	return out
}

func instanceLabel(p render.Profile) string {
	if p.Instance == "" {
		return "默认实例"
	}
	return "实例 " + p.Instance
}

// hostConflicts 检查本组（档案 p、四层代理 streams）与 hostKeys 上其他节点所属实例的冲突。
//
// enrollingSelf 为 nil 表示检查本组已有节点（修改档案或四层代理）：本组自己的节点不算冲突。
// 非 nil 表示有节点正在注册进本组：本组已在该主机上的其他节点算冲突（同一实例的第二份安装），
// *enrollingSelf 是重新注册的节点自身（为 0 表示新节点）。
func (d *Service) hostConflicts(groupID uint, p render.Profile, streams []render.StreamProxy, hostKeys []string, enrollingSelf *uint) error {
	if len(hostKeys) == 0 {
		return nil
	}
	q := d.q()
	others, err := q.Node.WithContext(bg).Where(q.Node.HostKey.In(hostKeys...)).Find()
	if err != nil {
		return err
	}
	mine := hostPorts(p, streams)
	cache := map[uint]struct {
		g       *store.NodeGroup
		p       render.Profile
		streams []render.StreamProxy
	}{}
	var problems []string
	for _, n := range others {
		if n.GroupID == groupID {
			if enrollingSelf != nil && n.ID != *enrollingSelf {
				// 同组另一个节点已在这台主机上：再注册一个就是同一实例的第二份安装
				problems = append(problems, fmt.Sprintf("该主机上已有本节点组的节点 %s（同一实例不能在一台主机上安装两份）", n.Name))
			}
			continue
		}
		c, ok := cache[n.GroupID]
		if !ok {
			g, err := store.First[store.NodeGroup](d.Store, n.GroupID)
			if err != nil {
				return err
			}
			op, err := groupProfile(g)
			if err != nil {
				return err
			}
			os, err := d.groupStreams(n.GroupID)
			if err != nil {
				return err
			}
			c.g, c.p, c.streams = g, op, os
			cache[n.GroupID] = c
		}
		if c.p.Instance == p.Instance {
			problems = append(problems, fmt.Sprintf("节点 %s 所在主机已运行%s（节点组 %s）", n.Name, instanceLabel(c.p), c.g.Name))
			continue
		}
		theirs := hostPorts(c.p, c.streams)
		for k, use := range mine {
			if other, clash := theirs[k]; clash {
				problems = append(problems, fmt.Sprintf("端口 %s（%s）与节点 %s 上%s（节点组 %s）的%s冲突", k, use, n.Name, instanceLabel(c.p), c.g.Name, other))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	problems = dedup(problems)
	return fmt.Errorf("%w: %s", ErrHostConflict, strings.Join(problems, "；"))
}

func dedup(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
