package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// 灰度发布（规格书 12.3 P9，授权功能 canary）：新版本按批次下发到部分节点，每批观察一段时间，
// 比较这些节点的 5xx 占比；超过阈值即把已灰度的节点回滚到原版本并停止，节点组的线上版本保持不变。
// 全部批次通过后节点组线上版本切换为新版本。灰度期间该节点组不能进行普通发布。

// CanaryOptions 是灰度参数。
type CanaryOptions struct {
	Percents      []int         `json:"percents"`        // 累计百分比，例如 [10, 50, 100]
	Observe       time.Duration `json:"observe"`         // 每批观察时长
	Max5xxPercent float64       `json:"max_5xx_percent"` // 5xx 占比阈值
	MinRequests   uint64        `json:"min_requests"`    // 观察期内请求少于此数时不判定（视为通过）
}

// CanaryBatch 是一批的结果。
type CanaryBatch struct {
	Nodes        []uint  `json:"nodes"`
	DeploymentID uint    `json:"deployment_id"`
	Status       string  `json:"status"` // pending | deploying | observing | passed | failed | rolled_back | skipped
	Requests     uint64  `json:"requests"`
	Errors5xx    uint64  `json:"errors_5xx"`
	Rate         float64 `json:"rate"`
	Message      string  `json:"message"`
}

// CanaryView 是灰度详情。
type CanaryView struct {
	*store.CanaryRelease
	Options CanaryOptions `json:"options"`
	Batches []CanaryBatch `json:"batches"`
}

var (
	ErrCanaryRunning = errors.New("a canary release is running for this node group")
	canaryActive     sync.Map // group id -> struct{}
)

func (d *Service) canaryRunning(groupID uint) bool {
	_, ok := canaryActive.Load(groupID)
	return ok
}

// canaryBatches 按累计百分比切分节点（每批至少 1 台，最后一批包含剩余全部）。
func canaryBatches(ids []uint, percents []int) [][]uint {
	var out [][]uint
	done := 0
	for i, p := range percents {
		n := (len(ids)*p + 99) / 100
		if i == len(percents)-1 || n > len(ids) {
			n = len(ids)
		}
		if n <= done {
			n = done + 1
		}
		if n > len(ids) {
			break
		}
		out = append(out, ids[done:n])
		done = n
		if done == len(ids) {
			break
		}
	}
	if done < len(ids) {
		out = append(out, ids[done:])
	}
	return out
}

// StartCanary 生成新版本并开始灰度。开启发布审批时不可用（灰度本身不替代双人复核）。
func (d *Service) StartCanary(groupID uint, note, user, ip string, o CanaryOptions) (*store.CanaryRelease, error) {
	if d.ApprovalEnabled() {
		return nil, ErrApprovalRequired
	}
	if len(o.Percents) == 0 {
		o.Percents = []int{10, 50, 100}
	}
	for i, p := range o.Percents {
		if p < 1 || p > 100 || (i > 0 && p <= o.Percents[i-1]) {
			return nil, errors.New("批次百分比必须递增且在 1–100 之间")
		}
	}
	if o.Observe <= 0 {
		o.Observe = 5 * time.Minute
	}
	if o.Max5xxPercent <= 0 {
		o.Max5xxPercent = 5
	}
	if o.MinRequests == 0 {
		o.MinRequests = 50
	}
	if _, loaded := canaryActive.LoadOrStore(groupID, struct{}{}); loaded {
		return nil, ErrCanaryRunning
	}
	release := func() { canaryActive.Delete(groupID) }
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		release()
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		lk.Unlock()
		release()
		return nil, err
	}
	_, digest, err := d.Preview(groupID)
	if err == nil && g.CurrentRevisionID != "" {
		if cur, e := d.revision(g.CurrentRevisionID, 0); e == nil && cur.Digest == digest {
			err = ErrNoChanges
		}
	}
	var rev *store.Revision
	if err == nil {
		rev, err = d.newRevision(groupID, g.CurrentRevisionID, note, user)
	}
	lk.Unlock()
	if err != nil {
		release()
		return nil, err
	}
	q := d.q()
	nodes, err := q.Node.WithContext(bg).Where(q.Node.GroupID.Eq(groupID), q.Node.Status.Neq("revoked")).Order(q.Node.ID).Find()
	if err != nil || len(nodes) < 2 {
		release()
		if err == nil {
			err = errors.New("灰度发布需要至少 2 个节点；节点更少时请直接发布")
		}
		return nil, err
	}
	ids := make([]uint, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	var batches []CanaryBatch
	for _, b := range canaryBatches(ids, o.Percents) {
		batches = append(batches, CanaryBatch{Nodes: b, Status: "pending"})
	}
	oj, _ := json.Marshal(o)
	bj, _ := json.Marshal(batches)
	c := &store.CanaryRelease{GroupID: groupID, RevisionID: rev.ID, BaseRevisionID: g.CurrentRevisionID, Status: "running", Options: string(oj),
		Batches: string(bj), Note: note, CreatedBy: user}
	if err := q.CanaryRelease.WithContext(bg).Create(c); err != nil {
		release()
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.canary_start", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]any{"canary": c.ID, "revision": rev.ID, "base": g.CurrentRevisionID, "options": o, "batches": len(batches)}})
	go func() {
		defer release()
		d.runCanary(c, rev, o, batches)
	}()
	return c, nil
}

func (d *Service) runCanary(c *store.CanaryRelease, rev *store.Revision, o CanaryOptions, batches []CanaryBatch) {
	q := d.q()
	save := func(status, msg string, done bool) {
		bj, _ := json.Marshal(batches)
		cr := q.CanaryRelease
		sel := cr.WithContext(bg).Where(cr.ID.Eq(c.ID))
		if done {
			// 先撤销“灰度进行中”标记，再写最终状态：看到结束状态的调用方随即发布时不会被误拒
			canaryActive.Delete(c.GroupID)
			_, _ = sel.UpdateSimple(cr.Batches.Value(string(bj)), cr.Status.Value(status), cr.Message.Value(msg), cr.FinishedAt.Value(time.Now()))
		} else {
			_, _ = sel.UpdateSimple(cr.Batches.Value(string(bj)), cr.Status.Value(status), cr.Message.Value(msg))
		}
	}
	deploy := func(r *store.Revision, kind, note string, nodes []uint) (*store.Deployment, error) {
		g, err := store.First[store.NodeGroup](d.Store, c.GroupID)
		if err != nil {
			return nil, err
		}
		lk := d.groupLock(c.GroupID)
		for i := 0; !lk.TryLock(); i++ { // 等待上一批的发布 goroutine 释放锁
			if i > 600 {
				return nil, ErrBusy
			}
			time.Sleep(100 * time.Millisecond)
		}
		dep, err := d.start(lk, g, r, kind, note, c.CreatedBy, nodes)
		if err != nil {
			return nil, err
		}
		for {
			cur, err := store.First[store.Deployment](d.Store, dep.ID)
			if err != nil {
				return nil, err
			}
			if cur.Status != "APPLYING" {
				return cur, nil
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	var canaried []uint
	rollback := func(why string) {
		if base, err := d.revision(c.BaseRevisionID, c.GroupID); err == nil && len(canaried) > 0 {
			_, _ = deploy(base, "rollback", "灰度回滚："+why, canaried)
		}
	}
	for i := range batches {
		b := &batches[i]
		last := i == len(batches)-1
		b.Status = "deploying"
		save("running", "", false)
		kind := "canary"
		if last {
			kind = "publish" // 最后一批：发布完成后节点组线上版本切换为新版本
		}
		dep, err := deploy(rev, kind, fmt.Sprintf("灰度第 %d/%d 批：%s", i+1, len(batches), c.Note), b.Nodes)
		if err != nil || (dep.Status != "APPLIED" && dep.Status != "PENDING_NODES") {
			msg := "发布失败"
			if err != nil {
				msg += "：" + err.Error()
			} else {
				b.DeploymentID = dep.ID
				msg += "（" + dep.Status + "）"
			}
			b.Status, b.Message = "failed", msg
			canaried = append(canaried, b.Nodes...)
			rollback(msg)
			for j := i + 1; j < len(batches); j++ {
				batches[j].Status = "skipped"
			}
			save("failed", fmt.Sprintf("第 %d 批%s，已回滚到 %s", i+1, msg, c.BaseRevisionID), true)
			return
		}
		b.DeploymentID = dep.ID
		canaried = append(canaried, b.Nodes...)
		if last {
			b.Status = "passed"
			break
		}
		// 观察期：统计本批节点的请求与 5xx
		b.Status = "observing"
		save("running", "", false)
		since := time.Now().Add(-time.Minute).Unix()
		time.Sleep(o.Observe)
		mm := q.MetricMinute
		rows, _ := mm.WithContext(bg).Where(mm.NodeID.In(b.Nodes...), mm.Minute.Gte(since)).Find()
		for _, r := range rows {
			b.Requests += r.Requests
			b.Errors5xx += r.S5xx
		}
		if b.Requests > 0 {
			b.Rate = float64(b.Errors5xx) * 100 / float64(b.Requests)
		}
		if b.Requests >= o.MinRequests && b.Rate > o.Max5xxPercent {
			b.Status = "rolled_back"
			b.Message = fmt.Sprintf("5xx 占比 %.1f%% 超过阈值 %.1f%%", b.Rate, o.Max5xxPercent)
			rollback(b.Message)
			for j := i + 1; j < len(batches); j++ {
				batches[j].Status = "skipped"
			}
			save("rolled_back", fmt.Sprintf("第 %d 批%s，已自动回滚到 %s", i+1, b.Message, c.BaseRevisionID), true)
			_ = d.Audit.Write(audit.Entry{Username: c.CreatedBy, Action: "release.canary_rollback", ObjectType: "node_group", ObjectID: fmt.Sprint(c.GroupID),
				Result: "rolled_back", Detail: map[string]any{"canary": c.ID, "batch": i + 1, "rate": b.Rate, "requests": b.Requests}})
			return
		}
		b.Status = "passed"
		if b.Requests < o.MinRequests {
			b.Message = fmt.Sprintf("观察期请求 %d 次，少于 %d，未做比例判定", b.Requests, o.MinRequests)
		}
	}
	save("done", "全部批次通过，线上版本已切换为 "+rev.ID, true)
	_ = d.Audit.Write(audit.Entry{Username: c.CreatedBy, Action: "release.canary_done", ObjectType: "node_group", ObjectID: fmt.Sprint(c.GroupID),
		Detail: map[string]any{"canary": c.ID, "revision": rev.ID}})
}

// GetCanary 返回灰度详情。
func (d *Service) GetCanary(id uint) (*CanaryView, error) {
	cr := d.q().CanaryRelease
	row, err := cr.WithContext(bg).Where(cr.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	v := &CanaryView{CanaryRelease: row}
	_ = json.Unmarshal([]byte(row.Options), &v.Options)
	_ = json.Unmarshal([]byte(row.Batches), &v.Batches)
	return v, nil
}

// ListCanaries 列出节点组最近的灰度发布。
func (d *Service) ListCanaries(groupID uint) ([]*CanaryView, error) {
	cr := d.q().CanaryRelease
	sel := cr.WithContext(bg).Order(cr.ID.Desc()).Limit(20)
	if groupID != 0 {
		sel = sel.Where(cr.GroupID.Eq(groupID))
	}
	rows, err := sel.Find()
	if err != nil {
		return nil, err
	}
	out := []*CanaryView{}
	for _, r := range rows {
		v := &CanaryView{CanaryRelease: r}
		_ = json.Unmarshal([]byte(r.Options), &v.Options)
		_ = json.Unmarshal([]byte(r.Batches), &v.Batches)
		out = append(out, v)
	}
	return out, nil
}
