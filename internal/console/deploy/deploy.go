// Package deploy 是控制台的发布编排（规格书 7.2）：渲染 → 生成不可变版本 → 下发到组内节点 → 汇总结果；
// 回滚即重新下发历史版本；节点上线时自动对齐到组的当前版本。
package deploy

import (
	"context"
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
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/textdiff"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"gorm.io/gorm"
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
	var sites []store.Site
	d.Store.DB.Where("group_id = ? AND enabled = ?", groupID, true).Order("name").Find(&sites)
	for _, s := range sites {
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
	var ups []store.Upstream
	d.Store.DB.Where("group_id = ?", groupID).Order("name").Find(&ups)
	for _, u := range ups {
		var ru render.UpstreamGroup
		if err := json.Unmarshal([]byte(u.Spec), &ru); err != nil {
			return in, fmt.Errorf("upstream %s: %w", u.Name, err)
		}
		ru.Name = u.Name
		in.Upstreams = append(in.Upstreams, ru)
	}
	return in, nil
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
		var rev store.Revision
		if d.Store.DB.Where("id = ?", g.CurrentRevisionID).First(&rev).Error == nil {
			old, _ = render.ReadTarGz(rev.Bundle)
		}
	}
	out := []FileDiff{} // 无变更时序列化为 []，不是 null
	names := map[string]bool{}
	for n := range b.Files {
		names[n] = true
	}
	for n := range old {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		if n != render.ManifestName {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		nb, inNew := b.Files[n]
		ob, inOld := old[n]
		change := ""
		switch {
		case inNew && !inOld:
			change = "added"
		case !inNew && inOld:
			change = "removed"
		case string(nb) != string(ob):
			change = "modified"
		default:
			continue
		}
		diff := ""
		if strings.HasSuffix(n, ".key") {
			diff = "（私钥内容不显示）"
		} else if n != "mime.types" {
			diff = textdiff.Unified(n, string(ob), string(nb))
		}
		out = append(out, FileDiff{Path: n, Change: change, Diff: diff})
	}
	return out, b.Manifest.Digest, nil
}

func (d *Service) newRevision(groupID uint, parent, note, user string) (*store.Revision, error) {
	in, err := d.Input(groupID, "r000000")
	if err != nil {
		return nil, err
	}
	var rev *store.Revision
	err = d.Store.DB.Transaction(func(tx *gorm.DB) error {
		r := &store.Revision{ID: fmt.Sprintf("tmp-%d", time.Now().UnixNano()), GroupID: groupID, ParentID: parent, Note: note, CreatedBy: user}
		if err := tx.Create(r).Error; err != nil {
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
		if err := tx.Save(r).Error; err != nil {
			return err
		}
		rev = r
		return nil
	})
	return rev, err
}

// Publish 生成新版本并异步下发到组内所有节点，立即返回发布记录。
func (d *Service) Publish(groupID uint, note, user, ip string) (*store.Deployment, error) {
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
		var cur store.Revision
		if d.Store.DB.Where("id = ?", g.CurrentRevisionID).First(&cur).Error == nil && cur.Digest == digest {
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

// CertRefresh 证书续期后，以组的线上版本为基础只替换引用该证书的站点的证书文件，生成新版本并下发。
// 不夹带草稿中的其他改动，因此在授权只读时也可执行（规格书 8.6）；agent 侧会再次核对“只有证书变化”。
// 返回 nil, nil 表示该组线上版本中没有需要替换的证书文件。
func (d *Service) CertRefresh(groupID, certID uint, user, ip string) (*store.Deployment, error) {
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil || g.CurrentRevisionID == "" {
		lk.Unlock()
		return nil, err
	}
	var cur store.Revision
	if err := d.Store.DB.Where("id = ?", g.CurrentRevisionID).First(&cur).Error; err != nil {
		lk.Unlock()
		return nil, err
	}
	files, err := render.ReadTarGz(cur.Bundle)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	crt, key, err := d.Certs.Resolve(certID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	var sites []string
	for _, name := range d.Certs.SitesUsing(certID, groupID) {
		c, k := "certs/site-"+name+".crt", "certs/site-"+name+".key"
		if _, ok := files[c]; !ok { // 站点尚未发布过，下次正常发布时自然带上新证书
			continue
		}
		if string(files[c]) == crt && string(files[k]) == key {
			continue
		}
		files[c], files[k] = []byte(crt), []byte(key)
		sites = append(sites, name)
	}
	if len(sites) == 0 {
		lk.Unlock()
		return nil, nil
	}
	var rev *store.Revision
	err = d.Store.DB.Transaction(func(tx *gorm.DB) error {
		r := &store.Revision{ID: fmt.Sprintf("tmp-%d", time.Now().UnixNano()), GroupID: groupID, ParentID: cur.ID,
			Note: "证书续期：" + strings.Join(sites, "、"), CreatedBy: user}
		if err := tx.Create(r).Error; err != nil {
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
		if err := tx.Save(r).Error; err != nil {
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
		Detail: map[string]any{"revision": rev.ID, "parent": cur.ID, "sites": sites, "digest": rev.Digest}})
	return d.start(lk, g, rev, "cert_renew", rev.Note, user, nil)
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
	var rev store.Revision
	if err := d.Store.DB.Where("id = ? AND group_id = ?", revID, groupID).First(&rev).Error; err != nil {
		lk.Unlock()
		return nil, fmt.Errorf("revision %s not found in this node group", revID)
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.rollback", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]string{"to": rev.ID, "from": g.CurrentRevisionID}})
	return d.start(lk, g, &rev, "rollback", "回滚到 "+rev.ID, user, nil)
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
	var rev store.Revision
	if d.Store.DB.Where("id = ?", g.CurrentRevisionID).First(&rev).Error != nil {
		lk.Unlock()
		return
	}
	_, _ = d.start(lk, g, &rev, "align", "节点上线自动对齐", "system", []uint{nodeID})
}

func (d *Service) start(lk *sync.Mutex, g *store.NodeGroup, rev *store.Revision, kind, note, user string, only []uint) (*store.Deployment, error) {
	var nodes []store.Node
	q := d.Store.DB.Where("group_id = ? AND status <> ?", g.ID, "revoked")
	if only != nil {
		q = q.Where("id IN ?", only)
	}
	q.Order("id").Find(&nodes)
	dep := &store.Deployment{GroupID: g.ID, RevisionID: rev.ID, Kind: kind, Status: "APPLYING", Note: note, CreatedBy: user, StartedAt: time.Now()}
	for _, n := range nodes {
		dep.Nodes = append(dep.Nodes, store.DeploymentNode{NodeID: n.ID, NodeName: n.Name, Status: "PENDING"})
	}
	if err := d.Store.DB.Create(dep).Error; err != nil {
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
			upd := func(status, msg, stderr string, done bool) {
				m := map[string]any{"status": status, "message": msg, "stderr": stderr}
				if done {
					m["finished_at"] = time.Now()
				}
				d.Store.DB.Model(&store.DeploymentNode{}).Where("id = ?", dn.ID).Updates(m)
				results[i] = status
			}
			if !d.GW.Online(n.ID) {
				upd("OFFLINE", "节点离线，上线后自动对齐", "", true)
				return
			}
			upd("APPLYING", "", "", false)
			var cur store.Node
			d.Store.DB.First(&cur, n.ID)
			var res *agentv1.CommandResult
			var err error
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
				d.Store.DB.Model(&store.Node{}).Where("id = ?", n.ID).Update("applied_revision", rev.ID)
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
	now := time.Now()
	d.Store.DB.Model(&store.Deployment{}).Where("id = ?", dep.ID).Updates(map[string]any{"status": status, "finished_at": now})
	if (status == "APPLIED" || status == "PENDING_NODES") && dep.Kind != "align" {
		d.Store.DB.Model(&store.NodeGroup{}).Where("id = ?", g.ID).Update("current_revision_id", rev.ID)
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
