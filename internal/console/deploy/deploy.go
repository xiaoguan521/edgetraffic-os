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
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"gorm.io/gen/field"
)

// ApplyTimeout 是单个节点执行一次发布的超时。
const ApplyTimeout = 90 * time.Second

// DefaultMaxParallelNodes 是单次发布并发下发节点的默认上限，防止百节点级群组瞬时占满连接与数据库写入。
const DefaultMaxParallelNodes = 20

// Service 负责发布与回滚。
type Service struct {
	Store     *store.Store
	Certs     *certs.Service
	GW        *gateway.Server
	Audit     *audit.Logger
	Log       *slog.Logger
	MimeTypes []byte

	Feature          func(name string) bool       // 授权功能检查（为空表示不检查）
	Agents           Agents                       // 数据面升级用；为空时使用 GW
	PackageKeys      map[string]ed25519.PublicKey // 升级包签名公钥；为空时使用内置公钥
	Box              *secretbox.Box               // JWT 规则密钥的加解密（P10）
	MaxParallelNodes int                          // 并发下发节点上限（<=0 时使用 DefaultMaxParallelNodes）

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
	prof, err := groupProfile(g)
	if err != nil {
		return render.Input{}, err
	}
	in := render.Input{Revision: rev, AllowedNet: allowed, RealIPFrom: realIP, MimeTypes: d.MimeTypes, Profile: prof}
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
		for i := range rs.Routes {
			if st := rs.Routes[i].Static; st != nil && st.BundleID != 0 {
				fs, err := d.staticFiles(groupID, st.BundleID)
				if err != nil {
					return in, fmt.Errorf("site %s: %w", s.Name, err)
				}
				cp := *st
				cp.Files = fs
				rs.Routes[i].Static = &cp
			}
		}
		if d.Certs != nil {
			if err := d.Certs.ResolveSite(&rs); err != nil {
				return in, fmt.Errorf("site %s: %w", s.Name, err)
			}
		}
		if err := d.OpenRules(&rs); err != nil {
			return in, fmt.Errorf("site %s: %w", s.Name, err)
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
	if in.IPSets, err = d.IPSets(groupID); err != nil {
		return in, fmt.Errorf("load ip sets: %w", err)
	}
	if in.HasGM() {
		if err := d.CheckGM(groupID); err != nil {
			return in, err
		}
	}
	if in.HasLua() {
		if err := d.CheckLua(groupID); err != nil {
			return in, err
		}
	}
	return in, nil
}

// ErrGMNotLicensed 表示站点启用了国密双证书，但授权不包含 gm_tls（企业版）。
var ErrGMNotLicensed = errors.New("LICENSE_FEATURE_DENIED: 当前授权不包含国密 TLCP（gm_tls），请去掉站点的国密证书")

// CheckGM：国密双证书只能下发到国密变体（铜锁构建）的节点，标准版 syedge -t 会拒绝这些指令。
func (d *Service) CheckGM(groupID uint) error {
	if d.Feature != nil && !d.Feature("gm_tls") {
		return ErrGMNotLicensed
	}
	nd := d.q().Node
	nodes, err := nd.WithContext(bg).Where(nd.GroupID.Eq(groupID), nd.Status.Neq("revoked")).Order(nd.Name).Find()
	if err != nil {
		return err
	}
	var std []string
	for _, n := range nodes {
		if !n.GM() {
			std = append(std, n.Name)
		}
	}
	if len(std) > 0 {
		return fmt.Errorf("GM_VARIANT_REQUIRED: 站点启用了国密双证书，但节点 %s 运行的不是国密版 %s；请先在“数据面升级”中把节点组升级到国密版", strings.Join(std, "、"), brand.ID())
	}
	return nil
}

// HasGMSites 报告节点组是否有启用国密双证书的站点（升级到非国密变体前检查）。
func (d *Service) HasGMSites(groupID uint) bool {
	q := d.q().Site
	sites, _ := q.WithContext(bg).Where(q.GroupID.Eq(groupID), q.Enabled.Is(true)).Find()
	for _, s := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(s.Spec), &rs) == nil && rs.HTTPS != nil && rs.HTTPS.GMCertID != 0 {
			return true
		}
	}
	return false
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
	redact := false
	g, _ := store.First[store.NodeGroup](d.Store, groupID)
	if g != nil && g.CurrentRevisionID != "" {
		if rev, err := d.revision(g.CurrentRevisionID, 0); err == nil {
			old, err = d.revisionFiles(rev)
			if err != nil {
				return nil, "", err
			}
			redact = isLegacyRevision(rev)
		}
	}
	var out []FileDiff
	if redact {
		out = diffFilesByHash(old, b.Files)
	} else {
		out = diffFiles(old, b.Files)
	}
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
	if err := d.ensureConfigManaged(groupID); err != nil {
		return nil, err
	}
	if d.ApprovalEnabled() {
		return nil, ErrApprovalRequired
	}
	if d.canaryRunning(groupID) {
		return nil, ErrCanaryRunning
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
	if err := d.ensureConfigManaged(groupID); err != nil {
		lk.Unlock()
		return nil, err
	}
	_, digest, err := d.Preview(groupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	pending, err := d.handoffPending(groupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if g.CurrentRevisionID != "" && !pending {
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
	if err := d.ensureConfigManaged(groupID); err != nil {
		lk.Unlock()
		return nil, err
	}
	if err := d.ensureHandoffComplete(groupID); err != nil {
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
	prof, err := groupProfile(g) // 国密路径按本组实例的版本目录改写
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	var changed []string
	if certID != 0 && d.Certs.Kind(certID) == store.KindSM2 {
		g, err := d.Certs.ResolveGM(certID)
		if err != nil {
			lk.Unlock()
			return nil, err
		}
		for _, name := range d.Certs.SitesUsing(certID, groupID) {
			if _, ok := files["certs/site-"+name+".sm2-sign.crt"]; !ok { // 尚未发布过国密证书
				continue
			}
			diff := false
			for suffix, pem := range render.GMFiles(g) {
				n := "certs/site-" + name + suffix
				if string(files[n]) != pem {
					files[n], diff = []byte(pem), true
				}
			}
			if diff {
				changed = append(changed, name)
			}
		}
	} else if certID != 0 {
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
		// 国密指令引用本版本目录的绝对路径，随新版本号改写（agent 只容许这一处改写）
		b := render.Repack(r.ID, render.RebaseRevisionFor(prof, files, cur.ID, r.ID))
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
	if err := d.ensureConfigManaged(g.ID); err != nil {
		lk.Unlock()
		return nil, err
	}
	if err := d.ensureHandoffComplete(g.ID); err != nil {
		lk.Unlock()
		return nil, err
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
	if err := d.ensureConfigManaged(groupID); err != nil {
		lk.Unlock()
		return nil, err
	}
	if err := d.ensureHandoffComplete(groupID); err != nil {
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
	if pending, err := d.handoffPending(g.ID); err != nil || pending {
		lk.Unlock()
		return
	}
	rev, err := d.revision(g.CurrentRevisionID, 0)
	if err != nil {
		lk.Unlock()
		return
	}
	if isLegacyRevision(rev) {
		_, _ = d.startLegacy(lk, g, rev, "align", "节点上线自动对齐已批准的兼容版本", "system", []uint{nodeID}, nil)
	} else {
		_, _ = d.start(lk, g, rev, "align", "节点上线自动对齐", "system", []uint{nodeID})
	}
}

func (d *Service) start(lk *sync.Mutex, g *store.NodeGroup, rev *store.Revision, kind, note, user string, only []uint) (*store.Deployment, error) {
	return d.startWithMode(lk, g, rev, kind, note, user, only, false, nil)
}

func (d *Service) startLegacy(lk *sync.Mutex, g *store.NodeGroup, rev *store.Revision, kind, note, user string, only []uint, before func(uint) error) (*store.Deployment, error) {
	return d.startWithMode(lk, g, rev, kind, note, user, only, true, before)
}

func (d *Service) startWithMode(lk *sync.Mutex, g *store.NodeGroup, rev *store.Revision, kind, note, user string, only []uint, legacy bool, before func(uint) error) (*store.Deployment, error) {
	mode, err := d.ManagementMode(g.ID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if legacy {
		if mode != ManagementModeLegacy || !isLegacyRevision(rev) {
			lk.Unlock()
			return nil, ErrLegacyModeRequired
		}
	} else if mode != ManagementModeManaged || isLegacyRevision(rev) {
		lk.Unlock()
		if isLegacyRevision(rev) {
			return nil, ErrLegacyAdminRequired
		}
		return nil, configManagementError(mode)
	}
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
	if before != nil {
		if err := before(dep.ID); err != nil {
			dp := d.q().Deployment
			_, _ = dp.WithContext(bg).Where(dp.ID.Eq(dep.ID)).UpdateSimple(dp.Status.Value("FAILED"), dp.FinishedAt.Value(time.Now()))
			lk.Unlock()
			return nil, err
		}
	}
	go func() {
		defer lk.Unlock()
		d.run(g, rev, dep, nodes)
	}()
	return dep, nil
}

func (d *Service) run(g *store.NodeGroup, rev *store.Revision, dep *store.Deployment, nodes []store.Node) {
	checks, checksErr := d.postChecks(rev)
	if checksErr != nil {
		d.Log.Error("failed to build post-check plan", "group", g.ID, "revision", rev.ID, "err", checksErr)
	}
	ag := d.agents()
	var wg sync.WaitGroup
	results := make([]string, len(nodes))
	limit := d.MaxParallelNodes
	if limit <= 0 {
		limit = DefaultMaxParallelNodes
	}
	sem := make(chan struct{}, limit)
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
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
			if !ag.Online(n.ID) {
				upd("OFFLINE", "节点离线，上线后自动对齐", "", true)
				return
			}
			upd("APPLYING", "", "", false)
			cur, err := q.Node.WithContext(bg).Where(q.Node.ID.Eq(n.ID)).First()
			if err != nil {
				upd("FAILED", "读取节点状态失败："+err.Error(), "", true)
				return
			}
			if checksErr != nil {
				// 不带探活下发等于放弃业务验证：宁可不发布，也不静默省略
				upd("FAILED", "无法生成探活计划，已停止下发："+checksErr.Error(), "", true)
				return
			}
			var res *agentv1.CommandResult
			if dep.Kind == "rollback" && !isLegacyRevision(rev) {
				// 回滚优先切换节点本地已有的版本：不依赖授权状态，过期时也可用（规格书 8.6）
				res, err = ag.Send(context.Background(), n.ID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Rollback{Rollback: &agentv1.Rollback{ToRevisionId: rev.ID}}}, ApplyTimeout)
				if err == nil && res.Status == agentv1.CommandResult_REJECTED && strings.Contains(res.Message, "not present") {
					res = nil // 本地没有该版本：退回到重新下发
				}
			}
			// Native versions must always use the signed digest in Apply. A local
			// rollback alone could execute files modified after the review.
			if res == nil && err == nil {
				msg := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{
					RevisionId: rev.ID, ParentRevisionId: cur.AppliedRevision, BundleSha256: rev.Digest, PostChecks: checks, TimeoutSeconds: int32(ApplyTimeout / time.Second),
				}}}
				if isLegacyRevision(rev) {
					if safe, ok := ag.(interface {
						SendLegacy(context.Context, uint, *agentv1.ServerMessage, time.Duration) (*agentv1.CommandResult, error)
					}); ok {
						res, err = safe.SendLegacy(context.Background(), n.ID, msg, ApplyTimeout)
					} else {
						err = gateway.ErrLegacyAgentUnsupported
					}
				} else {
					res, err = ag.Send(context.Background(), n.ID, msg, ApplyTimeout)
				}
			}
			if err != nil {
				st := "TIMEOUT"
				if errors.Is(err, gateway.ErrOffline) {
					st = "OFFLINE"
				} else if errors.Is(err, gateway.ErrLegacyAgentUnsupported) {
					st = "REJECTED"
				}
				upd(st, err.Error(), "", true)
				return
			}
			st := res.Status.String()
			if res.Status == agentv1.CommandResult_APPLIED {
				set := []field.AssignExpr{q.Node.AppliedRevision.Value(rev.ID)}
				if dep.Kind == "restore" || dep.Kind == "rollback" {
					set = append(set, q.Node.Drifted.Value(false), q.Node.TreeHash.Value(rev.Digest))
					_, _ = q.NodeDrift.WithContext(bg).Where(q.NodeDrift.NodeID.Eq(n.ID)).Delete()
				}
				_, _ = q.Node.WithContext(bg).Where(q.Node.ID.Eq(n.ID)).UpdateSimple(set...)
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
	dp := q.Deployment
	if (status == "APPLIED" || status == "PENDING_NODES") && dep.Kind != "align" && dep.Kind != "restore" && dep.Kind != "canary" {
		err := q.Transaction(func(tx *query.Query) error {
			ng := tx.NodeGroup
			if _, err := ng.WithContext(bg).Where(ng.ID.Eq(g.ID)).Update(ng.CurrentRevisionID, rev.ID); err != nil {
				return err
			}
			if dep.Kind == "publish" || (dep.Kind == "rollback" && isLegacyRevision(rev)) {
				kv := tx.SystemKV
				if _, err := kv.WithContext(bg).Where(kv.Key.Eq(handoffPendingKey(g.ID))).Delete(); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			status = "FAILED"
			d.Log.Error("failed to record deployed revision", "group", g.ID, "revision", rev.ID, "err", err)
		}
	}
	_, _ = dp.WithContext(bg).Where(dp.ID.Eq(dep.ID)).UpdateSimple(dp.Status.Value(status), dp.FinishedAt.Value(time.Now()))
	d.Log.Info("deployment finished", "id", dep.ID, "kind", dep.Kind, "revision", rev.ID, "status", status, "applied", applied, "failed", failed)
	_ = d.Audit.Write(audit.Entry{Username: dep.CreatedBy, Action: "release.result", ObjectType: "deployment", ObjectID: fmt.Sprint(dep.ID),
		Result: strings.ToLower(status), Detail: map[string]any{"revision": rev.ID, "kind": dep.Kind, "applied": applied, "failed": failed}})
}

// postChecks 为每个站点生成本机探活：请求首个域名的 /，只要求非 5xx。
// 冻结版本读不出来、或兼容包的探活元数据损坏时返回错误：空列表只表示“本来没有探活项”，
// 不能用来掩盖读取失败，否则业务探活会被悄悄省略。
func (d *Service) postChecks(rev *store.Revision) ([]*agentv1.PostCheck, error) {
	files, err := d.revisionFiles(rev)
	if err != nil {
		return nil, fmt.Errorf("读取版本 %s 失败：%w", rev.ID, err)
	}
	if isLegacyRevision(rev) {
		var metadata legacyMetadata
		if err := json.Unmarshal(files[configbundle.LegacyMarker], &metadata); err != nil {
			return nil, fmt.Errorf("版本 %s 的兼容包探活元数据无法解析：%w", rev.ID, err)
		}
		return metadata.Checks, nil
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
	return out, nil
}
