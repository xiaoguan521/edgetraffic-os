// Package deploy 是控制台的发布编排（规格书 7.2）：渲染 → 生成不可变版本 → 下发到组内节点 → 汇总结果；
// 回滚即重新下发历史版本；节点上线时自动对齐到组的当前版本。
package deploy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"gorm.io/gen/field"
)

// ApplyTimeout 是单个节点执行一次发布的超时。
const ApplyTimeout = 90 * time.Second

// Service 负责发布与回滚。
type Service struct {
	Store     *store.Store
	Certs     *certs.Service
	GW        *gateway.Server
	Audit     *audit.Logger
	Log       *slog.Logger
	MimeTypes []byte

	Feature     func(name string) bool       // 授权功能检查（为空表示不检查）
	Agents      Agents                       // 数据面升级用；为空时使用 GW
	PackageKeys map[string]ed25519.PublicKey // 升级包签名公钥；为空时使用内置公钥

	mu    sync.Mutex
	locks map[uint]*sync.Mutex
}

var (
	ErrNoChanges = errors.New("no changes since current revision")
	ErrBusy      = errors.New("another deployment is running for this node group")
)

func (d *Service) groupLock(id uint) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locks == nil {
		d.locks = map[uint]*sync.Mutex{}
	}
	if d.locks[id] == nil {
		d.locks[id] = &sync.Mutex{}
	}
	return d.locks[id]
}

func prefixes(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", s)
		}
		out = append(out, p)
	}
	return out, nil
}

// Input 从数据库组装渲染输入。
func (d *Service) Input(groupID uint, rev string) (render.Input, error) {
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return render.Input{}, err
	}
	allowed, err := prefixes(g.AllowedCIDRs)
	if err != nil {
		return render.Input{}, err
	}
	realIP, err := prefixes(g.RealIPFrom)
	if err != nil {
		return render.Input{}, err
	}
	in := render.Input{Revision: rev, AllowedNet: allowed, RealIPFrom: realIP, MimeTypes: d.MimeTypes}
	q := d.q()
	// 查询失败必须报错：否则会渲染出一份没有站点的配置并下发到全部节点
	sites, err := q.Site.WithContext(bg).Where(q.Site.GroupID.Eq(groupID), q.Site.Enabled.Is(true)).Order(q.Site.Name).Find()
	if err != nil {
		return in, fmt.Errorf("load sites: %w", err)
	}
	siteIDs := map[string]uint{}
	for _, s := range sites {
		siteIDs[s.Name] = s.ID
		var rs render.Site
		if err := json.Unmarshal([]byte(s.Spec), &rs); err != nil {
			return in, fmt.Errorf("site %s: %w", s.Name, err)
		}
		rs.Name = s.Name
		if d.Certs != nil {
			if err := d.Certs.ResolveSite(&rs); err != nil {
				return in, fmt.Errorf("site %s: %w", s.Name, err)
			}
		}
		in.Sites = append(in.Sites, rs)
	}
	if in.AcmeChallenges, err = d.challenges(); err != nil {
		return in, fmt.Errorf("load acme challenges: %w", err)
	}
	ups, err := q.Upstream.WithContext(bg).Where(q.Upstream.GroupID.Eq(groupID)).Order(q.Upstream.Name).Find()
	if err != nil {
		return in, fmt.Errorf("load upstreams: %w", err)
	}
	for _, u := range ups {
		var ru render.UpstreamGroup
		if err := json.Unmarshal([]byte(u.Spec), &ru); err != nil {
			return in, fmt.Errorf("upstream %s: %w", u.Name, err)
		}
		ru.Name = u.Name
		in.Upstreams = append(in.Upstreams, ru)
	}
	if err := d.applySnippets(groupID, &in, siteIDs); err != nil {
		return in, err
	}
	sts, err := q.StreamService.WithContext(bg).Where(q.StreamService.GroupID.Eq(groupID), q.StreamService.Enabled.Is(true)).Order(q.StreamService.Name).Find()
	if err != nil {
		return in, fmt.Errorf("load streams: %w", err)
	}
	if len(sts) > 0 && d.Feature != nil && !d.Feature("stream") {
		return in, ErrStreamNotLicensed
	}
	for _, st := range sts {
		var sp render.StreamProxy
		if err := json.Unmarshal([]byte(st.Spec), &sp); err != nil {
			return in, fmt.Errorf("stream %s: %w", st.Name, err)
		}
		sp.Name = st.Name
		in.Streams = append(in.Streams, sp)
	}
	return in, nil
}

// ErrStreamNotLicensed 表示节点组配置了四层代理，但授权不包含 stream 功能（规格书 6.5）。
var ErrStreamNotLicensed = errors.New("LICENSE_FEATURE_DENIED: 当前授权不包含四层代理（stream），请停用节点组中的四层代理服务")

// bg：发布在后台 goroutine 中执行，不随发起请求的结束而取消。
var bg = context.Background()

func (d *Service) q() *query.Query { return query.Use(d.Store.DB) }

// revision 读取一个版本；groupID 不为 0 时同时校验它属于该节点组。
func (d *Service) revision(id string, groupID uint) (*store.Revision, error) {
	rv := d.q().Revision
	sel := rv.WithContext(bg).Where(rv.ID.Eq(id))
	if groupID != 0 {
		sel = sel.Where(rv.GroupID.Eq(groupID))
	}
	return sel.First()
}

// FileDiff 是预览中的一个文件差异。
type FileDiff struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added | removed | modified
	Diff   string `json:"diff"`
}

// Preview 渲染当前草稿并与组的当前版本逐文件比较。私钥只显示是否变化，不显示内容。
func (d *Service) Preview(groupID uint) ([]FileDiff, string, error) {
	in, err := d.Input(groupID, "r000000")
	if err != nil {
		return nil, "", err
	}
	b, err := render.Render(in)
	if err != nil {
		return nil, "", err
	}
	old := map[string][]byte{}
	g, _ := store.First[store.NodeGroup](d.Store, groupID)
	if g != nil && g.CurrentRevisionID != "" {
		if rev, err := d.revision(g.CurrentRevisionID, 0); err == nil {
			old, _ = render.ReadTarGz(rev.Bundle)
		}
	}
	out := diffFiles(old, b.Files) // 无变更时序列化为 []，不是 null
	return out, b.Manifest.Digest, nil
}

func (d *Service) newRevision(groupID uint, parent, note, user string) (*store.Revision, error) {
	in, err := d.Input(groupID, "r000000")
	if err != nil {
		return nil, err
	}
	var rev *store.Revision
	err = d.q().Transaction(func(tx *query.Query) error {
		r := &store.Revision{ID: tempRevisionID(), GroupID: groupID, ParentID: parent, Note: note, CreatedBy: user}
		if err := tx.Revision.WithContext(bg).Create(r); err != nil {
			return err
		}
		r.ID = fmt.Sprintf("r%06d", r.Seq)
		in.Revision = r.ID
		b, err := render.Render(in)
		if err != nil {
			return err
		}
		tgz, err := b.TarGz()
		if err != nil {
			return err
		}
		mj, _ := json.Marshal(b.Manifest)
		r.Digest, r.Bundle, r.Manifest = b.Manifest.Digest, tgz, string(mj)
		if err := tx.Revision.WithContext(bg).Save(r); err != nil {
			return err
		}
		rev = r
		return nil
	})
	return rev, err
}

// Publish 生成新版本并异步下发到组内所有节点，立即返回发布记录。
// 开启发布审批时返回 ErrApprovalRequired，改走 RequestRelease → Approve。
func (d *Service) Publish(groupID uint, note, user, ip string) (*store.Deployment, error) {
	if d.ApprovalEnabled() {
		return nil, ErrApprovalRequired
	}
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	_, digest, err := d.Preview(groupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if g.CurrentRevisionID != "" {
		if cur, err := d.revision(g.CurrentRevisionID, 0); err == nil && cur.Digest == digest {
			lk.Unlock()
			return nil, ErrNoChanges
		}
	}
	rev, err := d.newRevision(groupID, g.CurrentRevisionID, note, user)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.publish", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]string{"revision": rev.ID, "parent": rev.ParentID, "note": note, "digest": rev.Digest}})
	return d.start(lk, g, rev, "publish", note, user, nil)
}

// ErrNeedsPublish 表示线上版本早于 ACME 支持（没有挑战片段），需要先正常发布一次。
var ErrNeedsPublish = errors.New("live revision predates ACME support; publish the node group once first")

// tempRevisionID 生成插入时用的临时版本号，随后在同一事务内改为 r%06d。
// 版本号列是 varchar(16)：PostgreSQL 会严格检查长度（SQLite 不会），临时值必须不超过 16 个字符。
func tempRevisionID() string {
	return fmt.Sprintf("t%015x", uint64(time.Now().UnixNano())&0xfffffffffffffff)
}

// challenges 返回进行中的 ACME 挑战（按 token 排序，保证渲染确定）。
func (d *Service) challenges() ([]render.AcmeChallenge, error) {
	ac := d.q().AcmeChallenge
	rows, err := ac.WithContext(bg).Order(ac.Token).Find()
	if err != nil {
		return nil, err
	}
	out := make([]render.AcmeChallenge, 0, len(rows))
	for _, r := range rows {
		out = append(out, render.AcmeChallenge{Token: r.Token, KeyAuth: r.KeyAuth})
	}
	return out, nil
}

// CertRefresh 证书续期后，以组的线上版本为基础只替换引用该证书的站点的证书文件，生成新版本并下发。
// 不夹带草稿中的其他改动，因此在授权只读时也可执行（规格书 8.6）；agent 侧会再次核对“只有证书变化”。
// 返回 nil, nil 表示该组线上版本中没有需要替换的文件。
func (d *Service) CertRefresh(groupID, certID uint, user, ip string) (*store.Deployment, error) {
	return d.RenewalRefresh(groupID, certID, user, ip)
}

// RenewalRefresh 以组的线上版本为基础，只更新续期类文件后生成新版本下发：
// certID 非 0 时替换引用该证书的站点的证书文件；同时把 ACME 挑战片段同步为当前进行中的挑战。
// 只有这两类文件会变化，因此在授权只读时也可执行，agent 会逐文件核对（规格书 8.6）。
func (d *Service) RenewalRefresh(groupID, certID uint, user, ip string) (*store.Deployment, error) {
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil || g.CurrentRevisionID == "" {
		lk.Unlock()
		return nil, err
	}
	cur, err := d.revision(g.CurrentRevisionID, 0)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	files, err := render.ReadTarGz(cur.Bundle)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	var changed []string
	if certID != 0 {
		crt, key, err := d.Certs.Resolve(certID)
		if err != nil {
			lk.Unlock()
			return nil, err
		}
		for _, name := range d.Certs.SitesUsing(certID, groupID) {
			c, k := "certs/site-"+name+".crt", "certs/site-"+name+".key"
			if _, ok := files[c]; !ok { // 站点尚未发布过，下次正常发布时自然带上新证书
				continue
			}
			if string(files[c]) == crt && string(files[k]) == key {
				continue
			}
			files[c], files[k] = []byte(crt), []byte(key)
			changed = append(changed, name)
		}
	}
	ch, err := d.challenges()
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if old, ok := files[render.AcmeSnippet]; ok {
		snip, err := render.RenderAcmeSnippet(ch)
		if err != nil {
			lk.Unlock()
			return nil, err
		}
		if string(old) != string(snip) {
			files[render.AcmeSnippet] = snip
			changed = append(changed, "ACME 验证")
		}
	} else if len(ch) > 0 {
		lk.Unlock()
		return nil, ErrNeedsPublish
	}
	if len(changed) == 0 {
		lk.Unlock()
		return nil, nil
	}
	var rev *store.Revision
	err = d.q().Transaction(func(tx *query.Query) error {
		r := &store.Revision{ID: tempRevisionID(), GroupID: groupID, ParentID: cur.ID,
			Note: "证书续期：" + strings.Join(changed, "、"), CreatedBy: user}
		if err := tx.Revision.WithContext(bg).Create(r); err != nil {
			return err
		}
		r.ID = fmt.Sprintf("r%06d", r.Seq)
		b := render.Repack(r.ID, files)
		tgz, err := b.TarGz()
		if err != nil {
			return err
		}
		mj, _ := json.Marshal(b.Manifest)
		r.Digest, r.Bundle, r.Manifest = b.Manifest.Digest, tgz, string(mj)
		if err := tx.Revision.WithContext(bg).Save(r); err != nil {
			return err
		}
		rev = r
		return nil
	})
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.cert_renew", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]any{"revision": rev.ID, "parent": cur.ID, "changed": changed, "digest": rev.Digest}})
	return d.start(lk, g, rev, "cert_renew", rev.Note, user, nil)
}

// Wait 等待发布结束（ACME 需要等挑战真正下发到节点后再通知 CA 验证）。
func (d *Service) Wait(ctx context.Context, depID uint) (*store.Deployment, error) {
	for {
		dp := d.q().Deployment
		dep, err := dp.WithContext(ctx).Where(dp.ID.Eq(depID)).First()
		if err != nil {
			return nil, err
		}
		if dep.Status != "APPLYING" {
			return dep, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ErrNotDrifted 表示节点当前没有漂移，无需恢复。
var ErrNotDrifted = errors.New("node has no manual changes to restore")

// Restore 把一台漂移节点恢复为它应有的版本（重新下发节点当前版本的原样内容，规格书 5.6）。
// 只作用于这一台节点，不改变节点组的当前版本；与回滚一样在授权只读时可用，agent 侧核对内容未变。
func (d *Service) Restore(nodeID uint, user, ip string) (*store.Deployment, error) {
	n, err := store.First[store.Node](d.Store, nodeID)
	if err != nil {
		return nil, err
	}
	if !n.Drifted {
		return nil, ErrNotDrifted
	}
	if n.AppliedRevision == "" {
		return nil, fmt.Errorf("节点 %s 还没有由控制台下发过版本", n.Name)
	}
	g, err := store.First[store.NodeGroup](d.Store, n.GroupID)
	if err != nil {
		return nil, err
	}
	lk := d.groupLock(g.ID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	rev, err := d.revision(n.AppliedRevision, g.ID)
	if err != nil {
		lk.Unlock()
		return nil, fmt.Errorf("revision %s not found", n.AppliedRevision)
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "node.restore", ObjectType: "node", ObjectID: n.Name,
		Detail: map[string]any{"revision": rev.ID, "tree_hash": n.TreeHash}})
	return d.start(lk, g, rev, "restore", "恢复 "+n.Name+" 的手工改动", user, []uint{n.ID})
}

// Rollback 把组回滚到某个历史版本（重新下发该版本）。
func (d *Service) Rollback(groupID uint, revID, user, ip string) (*store.Deployment, error) {
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	rev, err := d.revision(revID, groupID)
	if err != nil {
		lk.Unlock()
		return nil, fmt.Errorf("revision %s not found in this node group", revID)
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.rollback", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]string{"to": rev.ID, "from": g.CurrentRevisionID}})
	return d.start(lk, g, rev, "rollback", "回滚到 "+rev.ID, user, nil)
}

// Align 让刚上线的节点对齐到组的当前版本（规格书 7.2：离线节点上线后自动对齐）。
func (d *Service) Align(nodeID uint) {
	n, err := store.First[store.Node](d.Store, nodeID)
	if err != nil {
		return
	}
	g, err := store.First[store.NodeGroup](d.Store, n.GroupID)
	if err != nil || g.CurrentRevisionID == "" || g.CurrentRevisionID == n.AppliedRevision {
		return
	}
	lk := d.groupLock(g.ID)
	if !lk.TryLock() {
		return // 正在发布，结束后节点会被包含在发布中或下次上线再对齐
	}
	rev, err := d.revision(g.CurrentRevisionID, 0)
	if err != nil {
		lk.Unlock()
		return
	}
	_, _ = d.start(lk, g, rev, "align", "节点上线自动对齐", "system", []uint{nodeID})
}

func (d *Service) start(lk *sync.Mutex, g *store.NodeGroup, rev *store.Revision, kind, note, user string, only []uint) (*store.Deployment, error) {
	nd := d.q().Node
	sel := nd.WithContext(bg).Where(nd.GroupID.Eq(g.ID), nd.Status.Neq("revoked"))
	if only != nil {
		sel = sel.Where(nd.ID.In(only...))
	}
	ptrs, err := sel.Order(nd.ID).Find()
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	nodes := make([]store.Node, len(ptrs))
	for i, p := range ptrs {
		nodes[i] = *p
	}
	dep := &store.Deployment{GroupID: g.ID, RevisionID: rev.ID, Kind: kind, Status: "APPLYING", Note: note, CreatedBy: user, StartedAt: time.Now()}
	for _, n := range nodes {
		dep.Nodes = append(dep.Nodes, store.DeploymentNode{NodeID: n.ID, NodeName: n.Name, Status: "PENDING"})
	}
	if err := d.q().Deployment.WithContext(bg).Create(dep); err != nil {
		lk.Unlock()
		return nil, err
	}
	go func() {
		defer lk.Unlock()
		d.run(g, rev, dep, nodes)
	}()
	return dep, nil
}

func (d *Service) run(g *store.NodeGroup, rev *store.Revision, dep *store.Deployment, nodes []store.Node) {
	checks := d.postChecks(rev)
	var wg sync.WaitGroup
	results := make([]string, len(nodes))
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := nodes[i]
			dn := &dep.Nodes[i]
			q := d.q()
			upd := func(status, msg, stderr string, done bool) {
				dnq := q.DeploymentNode
				set := []field.AssignExpr{dnq.Status.Value(status), dnq.Message.Value(msg), dnq.Stderr.Value(stderr)}
				if done {
					set = append(set, dnq.FinishedAt.Value(time.Now()))
				}
				_, _ = dnq.WithContext(bg).Where(dnq.ID.Eq(dn.ID)).UpdateSimple(set...)
				results[i] = status
			}
			if !d.GW.Online(n.ID) {
				upd("OFFLINE", "节点离线，上线后自动对齐", "", true)
				return
			}
			upd("APPLYING", "", "", false)
			cur, err := q.Node.WithContext(bg).Where(q.Node.ID.Eq(n.ID)).First()
			if err != nil {
				upd("FAILED", "读取节点状态失败："+err.Error(), "", true)
				return
			}
			var res *agentv1.CommandResult
			if dep.Kind == "rollback" {
				// 回滚优先切换节点本地已有的版本：不依赖授权状态，过期时也可用（规格书 8.6）
				res, err = d.GW.Send(context.Background(), n.ID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Rollback{Rollback: &agentv1.Rollback{ToRevisionId: rev.ID}}}, ApplyTimeout)
				if err == nil && res.Status == agentv1.CommandResult_REJECTED && strings.Contains(res.Message, "not present") {
					res = nil // 本地没有该版本：退回到重新下发
				}
			}
			if res == nil && err == nil {
				res, err = d.GW.Send(context.Background(), n.ID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{
					RevisionId: rev.ID, ParentRevisionId: cur.AppliedRevision, BundleSha256: rev.Digest, PostChecks: checks, TimeoutSeconds: int32(ApplyTimeout / time.Second),
				}}}, ApplyTimeout)
			}
			if err != nil {
				st := "TIMEOUT"
				if errors.Is(err, gateway.ErrOffline) {
					st = "OFFLINE"
				}
				upd(st, err.Error(), "", true)
				return
			}
			st := res.Status.String()
			if res.Status == agentv1.CommandResult_APPLIED {
				_, _ = q.Node.WithContext(bg).Where(q.Node.ID.Eq(n.ID)).Update(q.Node.AppliedRevision, rev.ID)
			}
			upd(st, res.Message, res.Stderr, true)
		}(i)
	}
	wg.Wait()

	applied, failed := 0, 0
	for _, r := range results {
		switch r {
		case "APPLIED":
			applied++
		case "OFFLINE":
		default:
			failed++
		}
	}
	status := "APPLIED"
	switch {
	case failed > 0 && applied > 0:
		status = "PARTIAL"
	case failed > 0 || (applied == 0 && len(nodes) > 0 && dep.Kind != "align"):
		status = "FAILED"
		if failed == 0 {
			status = "PENDING_NODES" // 全部离线：版本已生成，节点上线后对齐
		}
	}
	q := d.q()
	dp, ng := q.Deployment, q.NodeGroup
	_, _ = dp.WithContext(bg).Where(dp.ID.Eq(dep.ID)).UpdateSimple(dp.Status.Value(status), dp.FinishedAt.Value(time.Now()))
	if (status == "APPLIED" || status == "PENDING_NODES") && dep.Kind != "align" && dep.Kind != "restore" {
		_, _ = ng.WithContext(bg).Where(ng.ID.Eq(g.ID)).Update(ng.CurrentRevisionID, rev.ID)
	}
	d.Log.Info("deployment finished", "id", dep.ID, "kind", dep.Kind, "revision", rev.ID, "status", status, "applied", applied, "failed", failed)
	_ = d.Audit.Write(audit.Entry{Username: dep.CreatedBy, Action: "release.result", ObjectType: "deployment", ObjectID: fmt.Sprint(dep.ID),
		Result: strings.ToLower(status), Detail: map[string]any{"revision": rev.ID, "kind": dep.Kind, "applied": applied, "failed": failed}})
}

// postChecks 为每个站点生成本机探活：请求首个域名的 /，只要求非 5xx。
func (d *Service) postChecks(rev *store.Revision) []*agentv1.PostCheck {
	files, err := render.ReadTarGz(rev.Bundle)
	if err != nil {
		return nil
	}
	var out []*agentv1.PostCheck
	for name, content := range files {
		if !strings.HasPrefix(name, "conf.d/site-") {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "server_name") {
				f := strings.Fields(strings.TrimSuffix(line, ";"))
				if len(f) > 1 {
					host := f[1]
					if strings.HasPrefix(host, "*.") { // 通配域名用一个具体子域探活
						host = "probe." + host[2:]
					}
					out = append(out, &agentv1.PostCheck{Host: host, Path: "/"})
				}
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
