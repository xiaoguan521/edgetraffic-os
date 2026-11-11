package deploy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// 节点组的删除（生命周期的最后一步）。删除只在控制台侧进行，不向任何节点下发命令：
// 组内必须已没有未吊销的节点，因此不会有数据面受影响；进行中的发布、审批、灰度与升级都会阻止删除。

var (
	ErrGroupHasNodes = errors.New("节点组内还有未吊销的节点")
	ErrGroupBusy     = errors.New("节点组有进行中的操作")
	ErrLastGroup     = errors.New("不能删除最后一个节点组")
)

// GroupUsage 是删除前展示给用户的占用情况：哪些会阻止删除，哪些会随组一起删除。
type GroupUsage struct {
	Nodes         int64  `json:"nodes"`         // 未吊销的节点：阻止删除
	RevokedNodes  int64  `json:"revoked_nodes"` // 已吊销的节点记录：随组删除（证书早已失效）
	Sites         int64  `json:"sites"`
	Upstreams     int64  `json:"upstreams"`
	Streams       int64  `json:"streams"`
	Snippets      int64  `json:"snippets"`
	IPSets        int64  `json:"ip_sets"`
	StaticBundles int64  `json:"static_bundles"`
	Imports       int64  `json:"imports"`
	Revisions     int64  `json:"revisions"`
	Deployments   int64  `json:"deployments"`
	Busy          string `json:"busy,omitempty"` // 非空：进行中的操作，阻止删除
	Last          bool   `json:"last"`           // 最后一个节点组：阻止删除
}

func groupUsage(ctx context.Context, q *query.Query, id uint) (*GroupUsage, error) {
	u := &GroupUsage{}
	nd := q.Node
	counts := []struct {
		dst *int64
		f   func() (int64, error)
	}{
		{&u.Nodes, func() (int64, error) {
			return nd.WithContext(ctx).Where(nd.GroupID.Eq(id), nd.Status.Neq("revoked")).Count()
		}},
		{&u.RevokedNodes, func() (int64, error) {
			return nd.WithContext(ctx).Where(nd.GroupID.Eq(id), nd.Status.Eq("revoked")).Count()
		}},
		{&u.Sites, func() (int64, error) { return q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(id)).Count() }},
		{&u.Upstreams, func() (int64, error) { return q.Upstream.WithContext(ctx).Where(q.Upstream.GroupID.Eq(id)).Count() }},
		{&u.Streams, func() (int64, error) {
			return q.StreamService.WithContext(ctx).Where(q.StreamService.GroupID.Eq(id)).Count()
		}},
		{&u.Snippets, func() (int64, error) { return q.Snippet.WithContext(ctx).Where(q.Snippet.GroupID.Eq(id)).Count() }},
		{&u.IPSets, func() (int64, error) { return q.IPSet.WithContext(ctx).Where(q.IPSet.GroupID.Eq(id)).Count() }},
		{&u.StaticBundles, func() (int64, error) {
			return q.StaticBundle.WithContext(ctx).Where(q.StaticBundle.GroupID.Eq(id)).Count()
		}},
		{&u.Imports, func() (int64, error) {
			return q.ConfigImport.WithContext(ctx).Where(q.ConfigImport.GroupID.Eq(id)).Count()
		}},
		{&u.Revisions, func() (int64, error) { return q.Revision.WithContext(ctx).Where(q.Revision.GroupID.Eq(id)).Count() }},
		{&u.Deployments, func() (int64, error) { return q.Deployment.WithContext(ctx).Where(q.Deployment.GroupID.Eq(id)).Count() }},
	}
	for _, c := range counts {
		n, err := c.f()
		if err != nil {
			return nil, err
		}
		*c.dst = n
	}
	busy := []struct {
		what string
		f    func() (int64, error)
	}{
		{"正在发布", func() (int64, error) {
			return q.Deployment.WithContext(ctx).Where(q.Deployment.GroupID.Eq(id), q.Deployment.Status.Eq("APPLYING")).Count()
		}},
		{"有待审批的发布申请", func() (int64, error) {
			return q.ReleaseRequest.WithContext(ctx).Where(q.ReleaseRequest.GroupID.Eq(id), q.ReleaseRequest.Status.Eq("pending")).Count()
		}},
		{"正在灰度发布", func() (int64, error) {
			return q.CanaryRelease.WithContext(ctx).Where(q.CanaryRelease.GroupID.Eq(id), q.CanaryRelease.Status.Eq("running")).Count()
		}},
		{"正在升级", func() (int64, error) {
			return q.DataplaneUpgrade.WithContext(ctx).Where(q.DataplaneUpgrade.GroupID.Eq(id), q.DataplaneUpgrade.Status.Eq("running")).Count()
		}},
	}
	for _, b := range busy {
		n, err := b.f()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			u.Busy = b.what
			break
		}
	}
	total, err := q.NodeGroup.WithContext(ctx).Count()
	if err != nil {
		return nil, err
	}
	u.Last = total <= 1
	return u, nil
}

// GroupUsage 返回节点组的占用情况（删除前预览）。
func (d *Service) GroupUsage(id uint) (*GroupUsage, error) {
	if _, err := store.First[store.NodeGroup](d.Store, id); err != nil {
		return nil, err
	}
	return groupUsage(context.Background(), d.q(), id)
}

// DeleteGroup 删除节点组及其全部配置与历史（站点、上游、四层服务、片段、IP 集、静态资源包、导入记录、版本、发布记录、
// 审批、灰度与升级记录、注册令牌、已吊销的节点记录）。证书库为全局资源，不受影响；审计日志保留。
func (d *Service) DeleteGroup(id uint, user, ip string) (*GroupUsage, error) {
	g, err := store.First[store.NodeGroup](d.Store, id)
	if err != nil {
		return nil, err
	}
	// 与发布、回滚、审批、灰度共用组锁；再取升级锁，删除期间不会有新的操作开始
	lk := d.groupLock(id)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	defer lk.Unlock()
	mu, _ := upgradeLocks.LoadOrStore(id, &sync.Mutex{})
	ulk := mu.(*sync.Mutex)
	if !ulk.TryLock() {
		return nil, fmt.Errorf("%w：正在升级", ErrGroupBusy)
	}
	defer ulk.Unlock()

	ctx := context.Background()
	var usage *GroupUsage
	err = d.q().Transaction(func(tx *query.Query) error {
		u, err := groupUsage(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case u.Last:
			return ErrLastGroup
		case u.Nodes > 0:
			return fmt.Errorf("%w：%d 个，请先吊销（吊销后节点证书立即失效，数据面继续以当前配置运行）", ErrGroupHasNodes, u.Nodes)
		case u.Busy != "":
			return fmt.Errorf("%w：%s", ErrGroupBusy, u.Busy)
		}
		usage = u

		var nodeIDs []uint
		if err := tx.Node.WithContext(ctx).Where(tx.Node.GroupID.Eq(id)).Pluck(tx.Node.ID, &nodeIDs); err != nil {
			return err
		}
		var depIDs []uint
		if err := tx.Deployment.WithContext(ctx).Where(tx.Deployment.GroupID.Eq(id)).Pluck(tx.Deployment.ID, &depIDs); err != nil {
			return err
		}
		if len(depIDs) > 0 {
			if _, err := tx.DeploymentNode.WithContext(ctx).Where(tx.DeploymentNode.DeploymentID.In(depIDs...)).Delete(); err != nil {
				return err
			}
		}
		if len(nodeIDs) > 0 {
			if _, err := tx.NodeDrift.WithContext(ctx).Where(tx.NodeDrift.NodeID.In(nodeIDs...)).Delete(); err != nil {
				return err
			}
			if _, err := tx.MetricMinute.WithContext(ctx).Where(tx.MetricMinute.NodeID.In(nodeIDs...)).Delete(); err != nil {
				return err
			}
		}
		dels := []func() error{
			func() error { _, err := tx.Node.WithContext(ctx).Where(tx.Node.GroupID.Eq(id)).Delete(); return err },
			func() error {
				_, err := tx.EnrollToken.WithContext(ctx).Where(tx.EnrollToken.GroupID.Eq(id)).Delete()
				return err
			},
			func() error { _, err := tx.Site.WithContext(ctx).Where(tx.Site.GroupID.Eq(id)).Delete(); return err },
			func() error {
				_, err := tx.Upstream.WithContext(ctx).Where(tx.Upstream.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.StreamService.WithContext(ctx).Where(tx.StreamService.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.Snippet.WithContext(ctx).Where(tx.Snippet.GroupID.Eq(id)).Delete()
				return err
			},
			func() error { _, err := tx.IPSet.WithContext(ctx).Where(tx.IPSet.GroupID.Eq(id)).Delete(); return err },
			func() error {
				_, err := tx.StaticBundle.WithContext(ctx).Where(tx.StaticBundle.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.ConfigImport.WithContext(ctx).Where(tx.ConfigImport.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.ReleaseRequest.WithContext(ctx).Where(tx.ReleaseRequest.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.CanaryRelease.WithContext(ctx).Where(tx.CanaryRelease.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.DataplaneUpgrade.WithContext(ctx).Where(tx.DataplaneUpgrade.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.Deployment.WithContext(ctx).Where(tx.Deployment.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.Revision.WithContext(ctx).Where(tx.Revision.GroupID.Eq(id)).Delete()
				return err
			},
			func() error {
				_, err := tx.SystemKV.WithContext(ctx).Where(tx.SystemKV.Key.Eq(managementModeKey(id))).Delete()
				return err
			},
			func() error {
				_, err := tx.NodeGroup.WithContext(ctx).Where(tx.NodeGroup.ID.Eq(id)).Delete()
				return err
			},
		}
		for _, del := range dels {
			if err := del(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "group.delete", ObjectType: "node_group", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"name": g.Name, "removed": usage}})
	return usage, nil
}
