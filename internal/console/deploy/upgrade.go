package deploy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
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

// UpgradeNode 是一台节点在升级中的状态。
type UpgradeNode struct {
	NodeID  uint   `json:"node_id"`
	Name    string `json:"name"`
	Batch   int    `json:"batch"`
	Status  string `json:"status"` // pending | upgrading | upgraded | rolled_back | failed | skipped | offline
	Message string `json:"message"`
}

// UpgradeView 是升级详情。
type UpgradeView struct {
	*store.DataplaneUpgrade
	Nodes []UpgradeNode `json:"nodes"`
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

// StartUpgrade 校验版本库后在后台分批执行升级。
func (d *Service) StartUpgrade(groupID uint, version, variant, user, ip string) (*store.DataplaneUpgrade, error) {
	if !pkgsig.VersionRe.MatchString(version) || !pkgsig.Variants[variant] {
		return nil, errors.New("版本号或变体无效")
	}
	if _, err := store.First[store.NodeGroup](d.Store, groupID); err != nil {
		return nil, err
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
			return nil, fmt.Errorf("节点 %s 的 agent 版本过旧（未上报架构），请先升级 agent", n.Name)
		}
		if c, _ := p.WithContext(bg).Where(p.Name.Eq("syapp"), p.Version.Eq(version), p.Arch.Eq(n.Arch), p.Variant.Eq(variant)).Count(); c == 0 {
			return nil, fmt.Errorf("版本库中没有 syapp %s %s %s 的升级包", version, n.Arch, variant)
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
		ids[i] = n.ID
		names[n.ID] = n.Name
	}
	var plan []UpgradeNode
	for bi, b := range planBatches(ids) {
		for _, id := range b {
			plan = append(plan, UpgradeNode{NodeID: id, Name: names[id], Batch: bi + 1, Status: "pending"})
		}
	}
	pj, _ := json.Marshal(plan)
	up := &store.DataplaneUpgrade{GroupID: groupID, Version: version, Variant: variant, Status: "running", Nodes: string(pj), CreatedBy: user}
	if err := q.DataplaneUpgrade.WithContext(bg).Create(up); err != nil {
		lk.Unlock()
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "dataplane.upgrade_start", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]any{"upgrade": up.ID, "version": version, "variant": variant, "nodes": len(plan)}})
	go func() {
		defer lk.Unlock()
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
	sigFor := func(arch string) []byte {
		p := q.DataplanePackage
		pkg, err := p.WithContext(bg).Select(p.Sig, p.SHA256).Where(p.Name.Eq("syapp"), p.Version.Eq(up.Version), p.Arch.Eq(arch), p.Variant.Eq(up.Variant)).First()
		if err != nil {
			return nil
		}
		return []byte(pkg.Sig)
	}
	ag := d.agents()
	failed := false
	for i := range plan {
		n := &plan[i]
		if failed {
			n.Status, n.Message = "skipped", "前面的节点失败，已停止"
			continue
		}
		node, err := store.First[store.Node](d.Store, n.NodeID)
		if err != nil || !ag.Online(n.NodeID) {
			n.Status, n.Message = "offline", "节点离线，未升级"
			failed = true // 离线节点也停止：避免部分节点在新旧版本间混跑而无人察觉
			save("running", false)
			continue
		}
		n.Status = "upgrading"
		save("running", false)
		sig := sigFor(node.Arch)
		var ps pkgsig.Sig
		_ = json.Unmarshal(sig, &ps)
		msg := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Upgrade{Upgrade: &agentv1.UpgradeDataplane{
			Action: agentv1.UpgradeDataplane_START, Version: up.Version, PackageSha256: ps.SHA256, SignatureJson: sig,
			PostChecks: d.groupPostChecks(up.GroupID), TimeoutSeconds: int32(UpgradeTimeout / time.Second)}}}
		res, err := ag.Send(context.Background(), n.NodeID, msg, UpgradeTimeout)
		switch {
		case err != nil:
			n.Status, n.Message, failed = "failed", err.Error(), true
		case res.Status == agentv1.CommandResult_APPLIED:
			c, err := ag.Send(context.Background(), n.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Upgrade{Upgrade: &agentv1.UpgradeDataplane{
				Action: agentv1.UpgradeDataplane_COMMIT, Version: up.Version}}}, time.Minute)
			if err != nil || c.Status != agentv1.CommandResult_APPLIED {
				msg := "提交失败"
				if err != nil {
					msg += "：" + err.Error()
				} else {
					msg += "：" + c.Message
				}
				n.Status, n.Message, failed = "failed", msg, true
			} else {
				n.Status, n.Message = "upgraded", res.Message
			}
		case res.Status == agentv1.CommandResult_ROLLED_BACK:
			n.Status, n.Message, failed = "rolled_back", res.Message, true
		default:
			n.Status, n.Message, failed = "failed", res.Status.String()+" "+res.Message, true
		}
		save("running", false)
	}
	status := "done"
	if failed {
		status = "failed"
	}
	save(status, true)
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
		out = append(out, v)
	}
	return out, nil
}

// AgentRestartWait 是 agent 自升级后等待节点以新版本重新上线的上限。
var AgentRestartWait = 90 * time.Second

// StartAgentUpgrade 分批升级节点组内的 agent（只重启 agent 进程，syapp 不受影响）。记录中 Variant 为 "agent"。
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
		if c, _ := p.WithContext(bg).Where(p.Name.Eq("syapp-agent"), p.Version.Eq(version), p.Arch.Eq(n.Arch)).Count(); c == 0 {
			return nil, fmt.Errorf("版本库中没有 syapp-agent %s %s 的升级包", version, n.Arch)
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
		pkg, err := p.WithContext(bg).Select(p.Sig, p.SHA256).Where(p.Name.Eq("syapp-agent"), p.Version.Eq(up.Version), p.Arch.Eq(node.Arch)).First()
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
			n.Status, n.Message, failed = "failed", "agent 未在规定时间内以新版本重新上线（syapp 不受影响）", true
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
