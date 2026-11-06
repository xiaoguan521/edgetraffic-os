package deploy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

// 数据面版本库与分批热升级（规格书 12.6）。
//
// 版本库：导入厂商签名的升级包（导入时验签）。升级：先 1 台、再 10%、再全量；每台节点 agent 下载包、
// 经 wrapper 验签与热升级、探活（失败时 agent 自动中止并回到旧版本），成功后控制台下发 COMMIT。
// 任一节点失败即停止后续节点。回退 = 以上一版本再做一次升级（同样不中断连接）。

// Agents 是升级用到的节点通信能力（生产为 gateway.Server，测试可替换）。
type Agents interface {
	Online(nodeID uint) bool
	Send(ctx context.Context, nodeID uint, m *agentv1.ServerMessage, timeout time.Duration) (*agentv1.CommandResult, error)
}

// UpgradeTimeout 是单台节点执行热升级（下载、验签、切换、探活）的上限。
const UpgradeTimeout = 3 * time.Minute

// UpgradeParallel 是同一批次内同时升级的节点数上限（批与批之间仍然串行）。
var UpgradeParallel = 10

var (
	ErrUpgradeRunning = errors.New("this node group already has a data-plane upgrade in progress")
	upgradeLocks      sync.Map // group id -> *sync.Mutex
)

func (d *Service) agents() Agents {
	if d.Agents != nil {
		return d.Agents
	}
	return d.GW
}

func (d *Service) packageKeys() map[string]ed25519.PublicKey {
	if d.PackageKeys != nil {
		return d.PackageKeys
	}
	return pkgsig.Keys()
}

// ImportPackage 验签并把升级包加入版本库。
func (d *Service) ImportPackage(data, sigJSON []byte, user, ip string) (*store.DataplanePackage, error) {
	sig, err := pkgsig.Parse(sigJSON)
	if err != nil {
		return nil, err
	}
	if err := pkgsig.Verify(sig, data, d.packageKeys()); err != nil {
		return nil, err
	}
	p := d.q().DataplanePackage
	if old, err := p.WithContext(bg).Where(p.Name.Eq(sig.Name), p.Version.Eq(sig.Version), p.Arch.Eq(sig.Arch), p.Variant.Eq(sig.Variant)).First(); err == nil {
		if old.SHA256 == sig.SHA256 {
			return old, nil
		}
		return nil, fmt.Errorf("版本库中已有 %s %s %s %s，内容不同；同一版本号不能替换", sig.Name, sig.Version, sig.Arch, sig.Variant)
	}
	row := &store.DataplanePackage{Name: sig.Name, Version: sig.Version, Arch: sig.Arch, Variant: sig.Variant, SHA256: sig.SHA256,
		Size: int64(len(data)), KID: sig.KID, Sig: string(sigJSON), Data: data, UploadedBy: user}
	if err := p.WithContext(bg).Create(row); err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "package.import", ObjectType: "package", ObjectID: fmt.Sprintf("%s-%s-%s-%s", sig.Name, sig.Version, sig.Arch, sig.Variant),
		Detail: map[string]any{"sha256": sig.SHA256, "kid": sig.KID, "size": len(data)}})
	return row, nil
}

// ListPackages 列出版本库（不含包内容）。
func (d *Service) ListPackages() ([]*store.DataplanePackage, error) {
	p := d.q().DataplanePackage
	return p.WithContext(bg).Select(p.ID, p.Name, p.Version, p.Arch, p.Variant, p.SHA256, p.Size, p.KID, p.UploadedBy, p.CreatedAt).
		Order(p.ID.Desc()).Find()
}

// UpgradeNode 是一个节点（实例）在升级中的状态。
type UpgradeNode struct {
	NodeID  uint   `json:"node_id"`
	Name    string `json:"name"`
	Batch   int    `json:"batch"`
	Status  string `json:"status"` // pending | upgrading | upgraded | rolled_back | failed | skipped | offline
	Message string `json:"message"`
	// 同机多实例（设计 0001 第 5.6 节）：数据面二进制按主机共用，升级以主机为单位
	Host     string `json:"host,omitempty"`      // 主机标识（HostKey 前 12 位；旧版 agent 不上报时每个节点自成一台主机）
	HostName string `json:"host_name,omitempty"` // 主机名（取该主机第一个节点上报的主机名）
	GroupID  uint   `json:"group_id,omitempty"`
	Sibling  bool   `json:"sibling,omitempty"` // 同机其他节点组的实例，随主机一起升级
}

// UpgradeHost 汇总一台主机的升级结果。
type UpgradeHost struct {
	Host   string `json:"host"`
	Name   string `json:"name"`
	Batch  int    `json:"batch"`
	Status string `json:"status"` // pending | upgrading | done | failed | partial | skipped
	Nodes  []uint `json:"nodes"`
}

// UpgradeView 是升级详情。
type UpgradeView struct {
	*store.DataplaneUpgrade
	Nodes []UpgradeNode `json:"nodes"`
	Hosts []UpgradeHost `json:"hosts"`
}

// planBatches：先 1 台，再约 10%（至少 1 台），其余全量。
func planBatches(ids []uint) [][]uint {
	if len(ids) == 0 {
		return nil
	}
	out := [][]uint{ids[:1]}
	rest := ids[1:]
	if len(rest) == 0 {
		return out
	}
	n := len(ids) / 10
	if n < 1 {
		n = 1
	}
	if n > len(rest) {
		n = len(rest)
	}
	out = append(out, rest[:n])
	if len(rest) > n {
		out = append(out, rest[n:])
	}
	return out
}

// hostKey 返回节点所在主机的标识；旧版 agent 不上报主机标识时，每个节点自成一台主机（与引入同机多实例之前的行为一致）。
func hostKey(n *store.Node) string {
	if n.HostKey != "" {
		return n.HostKey
	}
	return fmt.Sprintf("node-%d", n.ID)
}

func shortHost(k string) string {
	if len(k) > 12 && !strings.HasPrefix(k, "node-") {
		return k[:12]
	}
	return k
}

// upgradeParticipants 返回升级节点组 groupID 时涉及的全部节点：组内节点，加上这些节点所在主机上其他节点组的实例。
func (d *Service) upgradeParticipants(groupID uint) ([]*store.Node, error) {
	q := d.q()
	nodes, err := q.Node.WithContext(bg).Where(q.Node.GroupID.Eq(groupID), q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, n := range nodes {
		if n.HostKey != "" {
			keys = append(keys, n.HostKey)
		}
	}
	if len(keys) == 0 {
		return nodes, nil
	}
	sib, err := q.Node.WithContext(bg).Where(q.Node.HostKey.In(keys...), q.Node.GroupID.Neq(groupID), q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil {
		return nil, err
	}
	return append(nodes, sib...), nil
}

// checkUpgradable 校验版本库中有每个节点需要的升级包，以及各节点组的变体约束。
func (d *Service) checkUpgradable(nodes []*store.Node, version, variant string) error {
	if !pkgsig.VersionRe.MatchString(version) || !pkgsig.Variants[variant] {
		return errors.New("版本号或变体无效")
	}
	if len(nodes) == 0 {
		return errors.New("该节点组没有节点")
	}
	p := d.q().DataplanePackage
	gm := map[uint]bool{}
	for _, n := range nodes {
		if _, seen := gm[n.GroupID]; !seen {
			gm[n.GroupID] = d.HasGMSites(n.GroupID)
		}
		if variant != "gmssl" && gm[n.GroupID] {
			g, _ := store.First[store.NodeGroup](d.Store, n.GroupID)
			name := fmt.Sprint(n.GroupID)
			if g != nil {
				name = g.Name
			}
			return fmt.Errorf("节点组 %s 有启用国密双证书的站点，只能升级到国密版（gmssl）；同机实例共用一个数据面二进制，节点 %s 也在升级范围内。如需换成其他变体，请先去掉这些站点的国密证书并发布", name, n.Name)
		}
		if n.Arch == "" {
			return fmt.Errorf("节点 %s 的 agent 版本过旧（未上报架构），请先升级 agent", n.Name)
		}
		if c, _ := p.WithContext(bg).Where(p.Name.Eq(brand.ID()), p.Version.Eq(version), p.Arch.Eq(n.Arch), p.Variant.Eq(variant)).Count(); c == 0 {
			return fmt.Errorf("版本库中没有 %s %s %s %s 的升级包", brand.ID(), version, n.Arch, variant)
		}
	}
	// 装有 syedge-lua 的节点：模块与二进制版本绑定，必须同时有目标版本的附加包（与二进制一起切换）
	for _, n := range luaNodes(nodes) {
		if c, _ := p.WithContext(bg).Where(p.Name.Eq(brand.Lua()), p.Version.Eq(version), p.Arch.Eq(n.Arch), p.Variant.Eq("lua")).Count(); c == 0 {
			return fmt.Errorf("节点 %s 装有 %s 附加包：版本库中还需要 %s %s %s 的附加包", n.Name, brand.Lua(), brand.Lua(), version, n.Arch)
		}
	}
	return nil
}

// lockGroups 锁住升级涉及的全部节点组（同机实例跨组时，相关组都不能同时开始另一个升级）。
func lockGroups(nodes []*store.Node) (func(), error) {
	var ids []uint
	seen := map[uint]bool{}
	for _, n := range nodes {
		if !seen[n.GroupID] {
			seen[n.GroupID] = true
			ids = append(ids, n.GroupID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var held []*sync.Mutex
	unlock := func() {
		for _, m := range held {
			m.Unlock()
		}
	}
	for _, id := range ids {
		mu, _ := upgradeLocks.LoadOrStore(id, &sync.Mutex{})
		lk := mu.(*sync.Mutex)
		if !lk.TryLock() {
			unlock()
			return nil, ErrUpgradeRunning
		}
		held = append(held, lk)
	}
	return unlock, nil
}

// hostPlan 按主机编排：主机按先 1 台、再约 10%、其余全量分批；同一主机上的实例在同一批内依次升级，
// 本组的实例在前，同机其他组的实例在后（设计 0001 第 5.6 节）。
func hostPlan(nodes []*store.Node, groupID uint) []UpgradeNode {
	sorted := append([]*store.Node(nil), nodes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].GroupID == groupID, sorted[j].GroupID == groupID
		if a != b {
			return a
		}
		return sorted[i].ID < sorted[j].ID
	})
	var order []string
	members := map[string][]*store.Node{}
	for _, n := range sorted {
		k := hostKey(n)
		if _, ok := members[k]; !ok {
			order = append(order, k)
		}
		members[k] = append(members[k], n)
	}
	idx := make([]uint, len(order)) // planBatches 只看数量
	var plan []UpgradeNode
	pos := 0
	for bi, b := range planBatches(idx) {
		for range b {
			k := order[pos]
			pos++
			name := members[k][0].Hostname
			for _, n := range members[k] {
				plan = append(plan, UpgradeNode{NodeID: n.ID, Name: n.Name, Batch: bi + 1, Status: "pending",
					Host: shortHost(k), HostName: name, GroupID: n.GroupID, Sibling: n.GroupID != groupID})
			}
		}
	}
	return plan
}

// StartUpgrade 校验版本库后在后台按主机分批执行升级。
func (d *Service) StartUpgrade(groupID uint, version, variant, user, ip string) (*store.DataplaneUpgrade, error) {
	if !pkgsig.VersionRe.MatchString(version) || !pkgsig.Variants[variant] {
		return nil, errors.New("版本号或变体无效")
	}
	if _, err := store.First[store.NodeGroup](d.Store, groupID); err != nil {
		return nil, err
	}
	if variant != "gmssl" && d.HasGMSites(groupID) {
		return nil, errors.New("节点组有启用国密双证书的站点，只能升级到国密版（gmssl）；如需换成其他变体，请先去掉这些站点的国密证书并发布")
	}
	nodes, err := d.upgradeParticipants(groupID)
	if err != nil {
		return nil, err
	}
	if err := d.checkUpgradable(nodes, version, variant); err != nil {
		return nil, err
	}
	return d.launchUpgrade(groupID, version, variant, hostPlan(nodes, groupID), nodes, user, ip, "dataplane.upgrade_start", 0)
}

// RetryUpgrade 对一次已结束的升级中未完成的实例（失败、回退、跳过、离线）以同一版本重新执行，
// 用于“主机部分升级”后的“重试剩余实例”。已升级的实例不再执行。
func (d *Service) RetryUpgrade(id uint, user, ip string) (*store.DataplaneUpgrade, error) {
	prev, err := d.GetUpgrade(id)
	if err != nil {
		return nil, err
	}
	if prev.Variant == "agent" || prev.Status == "running" {
		return nil, errors.New("只能重试已结束的数据面升级")
	}
	var ids []uint
	for _, n := range prev.Nodes {
		if n.Status != "upgraded" {
			ids = append(ids, n.NodeID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("该升级的所有实例都已完成")
	}
	return d.restart(prev, ids, prev.Version, user, ip, "dataplane.upgrade_retry")
}

// RollbackHosts 把“主机部分升级”中已升级的实例按同样的热升级流程切到 version（通常是升级前的版本），
// 使主机上的实例回到同一版本。任何实例都不会停止。
func (d *Service) RollbackHosts(id uint, version, user, ip string) (*store.DataplaneUpgrade, error) {
	prev, err := d.GetUpgrade(id)
	if err != nil {
		return nil, err
	}
	if prev.Variant == "agent" || prev.Status == "running" {
		return nil, errors.New("只能回退已结束的数据面升级")
	}
	partial := map[string]bool{}
	for _, h := range prev.Hosts {
		if h.Status == "partial" {
			partial[h.Host] = true
		}
	}
	var ids []uint
	for _, n := range prev.Nodes {
		if partial[n.Host] && n.Status == "upgraded" {
			ids = append(ids, n.NodeID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("没有部分升级的主机")
	}
	return d.restart(prev, ids, version, user, ip, "dataplane.upgrade_rollback_hosts")
}

func (d *Service) restart(prev *UpgradeView, ids []uint, version, user, ip, action string) (*store.DataplaneUpgrade, error) {
	q := d.q()
	nodes, err := q.Node.WithContext(bg).Where(q.Node.ID.In(ids...), q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil {
		return nil, err
	}
	if err := d.checkUpgradable(nodes, version, prev.Variant); err != nil {
		return nil, err
	}
	return d.launchUpgrade(prev.GroupID, version, prev.Variant, hostPlan(nodes, prev.GroupID), nodes, user, ip, action, prev.ID)
}

func (d *Service) launchUpgrade(groupID uint, version, variant string, plan []UpgradeNode, nodes []*store.Node, user, ip, action string, from uint) (*store.DataplaneUpgrade, error) {
	unlock, err := lockGroups(nodes)
	if err != nil {
		return nil, err
	}
	pj, _ := json.Marshal(plan)
	up := &store.DataplaneUpgrade{GroupID: groupID, Version: version, Variant: variant, Status: "running", Nodes: string(pj), CreatedBy: user}
	if err := d.q().DataplaneUpgrade.WithContext(bg).Create(up); err != nil {
		unlock()
		return nil, err
	}
	hosts := map[string]bool{}
	for _, n := range plan {
		hosts[n.Host] = true
	}
	detail := map[string]any{"upgrade": up.ID, "version": version, "variant": variant, "nodes": len(plan), "hosts": len(hosts)}
	if from != 0 {
		detail["from_upgrade"] = from
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: action, ObjectType: "node_group", ObjectID: fmt.Sprint(groupID), Detail: detail})
	go func() {
		defer unlock()
		d.runUpgrade(up, plan)
	}()
	return up, nil
}

func (d *Service) runUpgrade(up *store.DataplaneUpgrade, plan []UpgradeNode) {
	q := d.q()
	save := func(status string, done bool) {
		pj, _ := json.Marshal(plan)
		u := q.DataplaneUpgrade
		sel := u.WithContext(bg).Where(u.ID.Eq(up.ID))
		if done {
			_, _ = sel.UpdateSimple(u.Nodes.Value(string(pj)), u.Status.Value(status), u.FinishedAt.Value(time.Now()))
		} else {
			_, _ = sel.UpdateSimple(u.Nodes.Value(string(pj)), u.Status.Value(status))
		}
	}
	sigOf := func(name, arch, variant string) []byte {
		p := q.DataplanePackage
		pkg, err := p.WithContext(bg).Select(p.Sig, p.SHA256).Where(p.Name.Eq(name), p.Version.Eq(up.Version), p.Arch.Eq(arch), p.Variant.Eq(variant)).First()
		if err != nil {
			return nil
		}
		return []byte(pkg.Sig)
	}
	ag := d.agents()
	var mu sync.Mutex // 保护 plan、failed 与 save
	failed := false
	set := func(n *UpgradeNode, status, msg string, fail bool) {
		mu.Lock()
		n.Status, n.Message = status, msg
		if fail {
			failed = true
		}
		save("running", false)
		mu.Unlock()
	}
	// upgradeOne 升级一个实例，返回是否成功（已提交）
	upgradeOne := func(n *UpgradeNode) bool {
		node, err := store.First[store.Node](d.Store, n.NodeID)
		if err != nil || !ag.Online(n.NodeID) {
			set(n, "offline", "节点离线，未升级", true) // 离线节点也停止：避免部分节点在新旧版本间混跑而无人察觉
			return false
		}
		set(n, "upgrading", "", false)
		sig := sigOf(brand.ID(), node.Arch, up.Variant)
		var ps pkgsig.Sig
		_ = json.Unmarshal(sig, &ps)
		msg := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Upgrade{Upgrade: &agentv1.UpgradeDataplane{
			Action: agentv1.UpgradeDataplane_START, Version: up.Version, PackageSha256: ps.SHA256, SignatureJson: sig,
			PostChecks: d.groupPostChecks(node.GroupID), TimeoutSeconds: int32(UpgradeTimeout / time.Second)}}}
		if node.LuaAddon != "" {
			msg.GetUpgrade().LuaSignatureJson = sigOf(brand.Lua(), node.Arch, "lua")
		}
		res, err := ag.Send(context.Background(), n.NodeID, msg, UpgradeTimeout)
		switch {
		case err != nil:
			set(n, "failed", err.Error(), true)
		case res.Status == agentv1.CommandResult_APPLIED:
			c, err := ag.Send(context.Background(), n.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Upgrade{Upgrade: &agentv1.UpgradeDataplane{
				Action: agentv1.UpgradeDataplane_COMMIT, Version: up.Version}}}, time.Minute)
			if err != nil || c.Status != agentv1.CommandResult_APPLIED {
				m := "提交失败"
				if err != nil {
					m += "：" + err.Error()
				} else {
					m += "：" + c.Message
				}
				set(n, "failed", m, true)
				return false
			}
			set(n, "upgraded", res.Message, false)
			return true
		case res.Status == agentv1.CommandResult_ROLLED_BACK:
			set(n, "rolled_back", res.Message, true)
		default:
			set(n, "failed", res.Status.String()+" "+res.Message, true)
		}
		return false
	}
	// upgradeHost：主机内的实例依次升级。第一个实例失败时 agent 已中止、软链切回，主机保持旧版本；
	// 之后的实例失败时该实例回到旧 master，主机处于“部分升级”，剩余实例不再执行（可重试剩余实例或整体回退）
	upgradeHost := func(h []*UpgradeNode) {
		for i, n := range h {
			if upgradeOne(n) {
				continue
			}
			why := "同机第一个实例升级失败，主机保持原版本"
			if i > 0 {
				why = "同机实例升级失败，主机部分升级：可重试剩余实例或整体回退"
			}
			mu.Lock()
			for _, r := range h[i+1:] {
				r.Status, r.Message = "skipped", why
			}
			save("running", false)
			mu.Unlock()
			return
		}
	}
	// 批与批之间串行；批内最多 UpgradeParallel 台主机同时升级。任一实例失败后，尚未开始的主机与后续批次都不再执行；
	// 已经开始的主机会把它的实例做完（不主动留下只升了一部分的主机）
	for bi := 1; ; bi++ {
		var hosts [][]*UpgradeNode
		at := map[string]int{}
		for i := range plan {
			if plan[i].Batch != bi {
				continue
			}
			k := plan[i].Host
			if _, ok := at[k]; !ok {
				at[k] = len(hosts)
				hosts = append(hosts, nil)
			}
			hosts[at[k]] = append(hosts[at[k]], &plan[i])
		}
		if len(hosts) == 0 {
			break
		}
		sem := make(chan struct{}, UpgradeParallel)
		var wg sync.WaitGroup
		for _, h := range hosts {
			sem <- struct{}{}
			mu.Lock()
			stop := failed
			if stop {
				for _, n := range h {
					n.Status, n.Message = "skipped", "前面的节点失败，已停止"
				}
			}
			mu.Unlock()
			if stop {
				<-sem
				continue
			}
			wg.Add(1)
			go func(h []*UpgradeNode) {
				defer func() { <-sem; wg.Done() }()
				upgradeHost(h)
			}(h)
		}
		wg.Wait()
	}
	status := "done"
	if failed {
		status = "failed"
		for _, h := range summarizeHosts(plan) {
			if h.Status == "partial" {
				status = "partial"
			}
		}
	}
	mu.Lock()
	save(status, true)
	mu.Unlock()
	ok := 0
	for _, n := range plan {
		if n.Status == "upgraded" {
			ok++
		}
	}
	d.Log.Info("dataplane upgrade finished", "upgrade", up.ID, "version", up.Version, "status", status, "upgraded", ok)
	_ = d.Audit.Write(audit.Entry{Username: up.CreatedBy, Action: "dataplane.upgrade_result", ObjectType: "node_group", ObjectID: fmt.Sprint(up.GroupID),
		Result: status, Detail: map[string]any{"upgrade": up.ID, "version": up.Version, "upgraded": ok, "nodes": plan}})
}

// summarizeHosts 汇总每台主机的升级结果：全部升级为 done；有的升级了、有的没有为 partial；
// 都没有升级时，有失败为 failed、都是跳过为 skipped。
func summarizeHosts(plan []UpgradeNode) []UpgradeHost {
	var out []UpgradeHost
	at := map[string]int{}
	for _, n := range plan {
		k := n.Host
		if k == "" {
			k = fmt.Sprintf("node-%d", n.NodeID) // 引入主机编排之前的升级记录
		}
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, UpgradeHost{Host: k, Name: n.HostName, Batch: n.Batch})
		}
		out[i].Nodes = append(out[i].Nodes, n.NodeID)
	}
	for i := range out {
		var upgraded, bad, busy, pending int
		for _, id := range out[i].Nodes {
			for _, n := range plan {
				if n.NodeID != id {
					continue
				}
				switch n.Status {
				case "upgraded":
					upgraded++
				case "upgrading":
					busy++
				case "pending":
					pending++
				case "skipped":
				default:
					bad++
				}
			}
		}
		total := len(out[i].Nodes)
		switch {
		case busy > 0 || (upgraded > 0 && pending > 0):
			out[i].Status = "upgrading"
		case upgraded == total:
			out[i].Status = "done"
		case upgraded > 0:
			out[i].Status = "partial"
		case bad > 0:
			out[i].Status = "failed"
		case pending == total:
			out[i].Status = "pending"
		default:
			out[i].Status = "skipped"
		}
	}
	return out
}

// groupPostChecks 返回节点组当前版本的探活项（与发布相同）。
func (d *Service) groupPostChecks(groupID uint) []*agentv1.PostCheck {
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil || g.CurrentRevisionID == "" {
		return nil
	}
	rev, err := d.revision(g.CurrentRevisionID, groupID)
	if err != nil {
		return nil
	}
	return d.postChecks(rev)
}

// GetUpgrade 返回升级详情。
func (d *Service) GetUpgrade(id uint) (*UpgradeView, error) {
	u := d.q().DataplaneUpgrade
	row, err := u.WithContext(bg).Where(u.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	v := &UpgradeView{DataplaneUpgrade: row, Nodes: []UpgradeNode{}}
	_ = json.Unmarshal([]byte(row.Nodes), &v.Nodes)
	v.Hosts = summarizeHosts(v.Nodes)
	return v, nil
}

// ListUpgrades 列出最近的升级。
func (d *Service) ListUpgrades(groupID uint) ([]*UpgradeView, error) {
	u := d.q().DataplaneUpgrade
	sel := u.WithContext(bg).Order(u.ID.Desc()).Limit(30)
	if groupID != 0 {
		sel = sel.Where(u.GroupID.Eq(groupID))
	}
	rows, err := sel.Find()
	if err != nil {
		return nil, err
	}
	out := []*UpgradeView{}
	for _, r := range rows {
		v := &UpgradeView{DataplaneUpgrade: r, Nodes: []UpgradeNode{}}
		_ = json.Unmarshal([]byte(r.Nodes), &v.Nodes)
		v.Hosts = summarizeHosts(v.Nodes)
		out = append(out, v)
	}
	return out, nil
}

// AgentRestartWait 是 agent 自升级后等待节点以新版本重新上线的上限。
var AgentRestartWait = 90 * time.Second

// StartAgentUpgrade 分批升级节点组内的 agent（只重启 agent 进程，syedge 不受影响）。记录中 Variant 为 "agent"。
func (d *Service) StartAgentUpgrade(groupID uint, version, user, ip string) (*store.DataplaneUpgrade, error) {
	if !pkgsig.VersionRe.MatchString(version) {
		return nil, errors.New("版本号无效")
	}
	q := d.q()
	nodes, err := q.Node.WithContext(bg).Where(q.Node.GroupID.Eq(groupID), q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("该节点组没有节点")
	}
	p := q.DataplanePackage
	for _, n := range nodes {
		if n.Arch == "" {
			return nil, fmt.Errorf("节点 %s 的 agent 版本过旧（未上报架构），请通过安装包升级", n.Name)
		}
		if c, _ := p.WithContext(bg).Where(p.Name.Eq(brand.Agent()), p.Version.Eq(version), p.Arch.Eq(n.Arch)).Count(); c == 0 {
			return nil, fmt.Errorf("版本库中没有 %s %s %s 的升级包", brand.Agent(), version, n.Arch)
		}
	}
	mu, _ := upgradeLocks.LoadOrStore(groupID, &sync.Mutex{})
	lk := mu.(*sync.Mutex)
	if !lk.TryLock() {
		return nil, ErrUpgradeRunning
	}
	ids := make([]uint, len(nodes))
	names := map[uint]string{}
	for i, n := range nodes {
		ids[i], names[n.ID] = n.ID, n.Name
	}
	var plan []UpgradeNode
	for bi, b := range planBatches(ids) {
		for _, id := range b {
			plan = append(plan, UpgradeNode{NodeID: id, Name: names[id], Batch: bi + 1, Status: "pending"})
		}
	}
	pj, _ := json.Marshal(plan)
	up := &store.DataplaneUpgrade{GroupID: groupID, Version: version, Variant: "agent", Status: "running", Nodes: string(pj), CreatedBy: user}
	if err := q.DataplaneUpgrade.WithContext(bg).Create(up); err != nil {
		lk.Unlock()
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "agent.upgrade_start", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]any{"upgrade": up.ID, "version": version, "nodes": len(plan)}})
	go func() {
		defer lk.Unlock()
		d.runAgentUpgrade(up, plan)
	}()
	return up, nil
}

func (d *Service) runAgentUpgrade(up *store.DataplaneUpgrade, plan []UpgradeNode) {
	q := d.q()
	ag := d.agents()
	failed := false
	save := func(status string, done bool) {
		pj, _ := json.Marshal(plan)
		u := q.DataplaneUpgrade
		set := u.WithContext(bg).Where(u.ID.Eq(up.ID))
		if done {
			_, _ = set.UpdateSimple(u.Nodes.Value(string(pj)), u.Status.Value(status), u.FinishedAt.Value(time.Now()))
		} else {
			_, _ = set.UpdateSimple(u.Nodes.Value(string(pj)), u.Status.Value(status))
		}
	}
	for i := range plan {
		n := &plan[i]
		if failed {
			n.Status, n.Message = "skipped", "前面的节点失败，已停止"
			continue
		}
		node, err := store.First[store.Node](d.Store, n.NodeID)
		if err != nil || !ag.Online(n.NodeID) {
			n.Status, n.Message, failed = "offline", "节点离线，未升级", true
			continue
		}
		if node.AgentVersion == up.Version {
			n.Status, n.Message = "upgraded", "已是该版本"
			continue
		}
		n.Status = "upgrading"
		save("running", false)
		p := q.DataplanePackage
		pkg, err := p.WithContext(bg).Select(p.Sig, p.SHA256).Where(p.Name.Eq(brand.Agent()), p.Version.Eq(up.Version), p.Arch.Eq(node.Arch)).First()
		if err != nil {
			n.Status, n.Message, failed = "failed", "版本库中没有对应的包", true
			continue
		}
		res, err := ag.Send(context.Background(), n.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_UpgradeAgent{UpgradeAgent: &agentv1.UpgradeAgent{
			Version: up.Version, PackageSha256: pkg.SHA256, SignatureJson: []byte(pkg.Sig)}}}, 2*time.Minute)
		if err != nil || res.Status != agentv1.CommandResult_APPLIED {
			msg := "agent 拒绝升级"
			if err != nil {
				msg = err.Error()
			} else {
				msg += "：" + res.Message
			}
			n.Status, n.Message, failed = "failed", msg, true
			save("running", false)
			continue
		}
		back := false
		deadline := time.Now().Add(AgentRestartWait)
		for time.Now().Before(deadline) {
			time.Sleep(time.Second)
			if nd, err := store.First[store.Node](d.Store, n.NodeID); err == nil && nd.AgentVersion == up.Version && ag.Online(n.NodeID) {
				back = true
				break
			}
		}
		if back {
			n.Status, n.Message = "upgraded", "已以新版本重新上线"
		} else {
			n.Status, n.Message, failed = "failed", "agent 未在规定时间内以新版本重新上线（"+brand.ID()+" 不受影响）", true
		}
		save("running", false)
	}
	status := "done"
	if failed {
		status = "failed"
	}
	save(status, true)
	_ = d.Audit.Write(audit.Entry{Username: up.CreatedBy, Action: "agent.upgrade_result", ObjectType: "node_group", ObjectID: fmt.Sprint(up.GroupID),
		Result: status, Detail: map[string]any{"upgrade": up.ID, "version": up.Version, "nodes": plan}})
}
