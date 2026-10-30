package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// newTestService 返回一个没有节点的发布服务：发布只生成版本、更新组的当前版本，不需要 agent。
func newTestService(t *testing.T) (*Service, *store.NodeGroup) {
	t.Helper()
	st := storetest.Open(t)
	d := &Service{Store: st, Audit: &audit.Logger{DB: st.DB}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MimeTypes: render.DefaultMimeTypes()}
	g := &store.NodeGroup{Name: "default"}
	if err := query.Use(st.DB).NodeGroup.WithContext(context.Background()).Create(g); err != nil {
		t.Fatal(err)
	}
	return d, g
}

func addSite(t *testing.T, d *Service, g *store.NodeGroup, name string) {
	t.Helper()
	spec, _ := json.Marshal(render.Site{Name: name, Domains: []string{name + ".example.com"},
		Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}})
	if err := query.Use(d.Store.DB).Site.WithContext(context.Background()).Create(&store.Site{GroupID: g.ID, Name: name, Spec: string(spec), Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func waitDone(t *testing.T, d *Service, dep *store.Deployment) *store.Deployment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.Wait(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 发布 goroutine 写完结果后才释放组锁；等锁释放，避免下一步操作得到 ErrBusy
	lk := d.groupLock(dep.GroupID)
	for !lk.TryLock() {
		if ctx.Err() != nil {
			t.Fatal("group lock not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	lk.Unlock()
	return out
}

func currentRev(t *testing.T, d *Service, g *store.NodeGroup) string {
	t.Helper()
	cur, err := store.First[store.NodeGroup](d.Store, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cur.CurrentRevisionID
}

func TestApprovalDisabledPublishesDirectly(t *testing.T) {
	d, g := newTestService(t)
	addSite(t, d, g, "site-a")
	dep, err := d.Publish(g.ID, "first", "alice", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if waitDone(t, d, dep).Status != "APPLIED" || currentRev(t, d, g) != dep.RevisionID {
		t.Fatal("publish without approval should apply immediately")
	}
}

func TestApprovalFlow(t *testing.T) {
	d, g := newTestService(t)
	if err := d.SetApproval(true, "admin", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	addSite(t, d, g, "site-a")

	// 开启审批后不能直接发布
	if _, err := d.Publish(g.ID, "x", "alice", "127.0.0.1"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("publish with approval on: %v", err)
	}
	req, err := d.RequestRelease(g.ID, "上线 a", "alice", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != ApprovalPending || req.RevisionID == "" || req.BaseRevisionID != "" {
		t.Fatalf("unexpected request %+v", req)
	}
	if currentRev(t, d, g) != "" {
		t.Fatal("request must not change the live revision")
	}
	// 同一组同时只能有一个待审批的请求
	if _, err := d.RequestRelease(g.ID, "again", "bob", "127.0.0.1"); !errors.Is(err, ErrPendingRequest) {
		t.Fatalf("second request: %v", err)
	}
	// 审批详情里能看到这次发布的差异
	detail, err := d.GetRequest(req.ID)
	if err != nil || len(detail.Files) == 0 {
		t.Fatalf("request detail: %v %+v", err, detail)
	}
	// 提交人不能自己批准
	if _, err := d.Approve(req.ID, "", "alice", "127.0.0.1"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	// 审批期间站点再被修改，批准后下发的仍是提交时冻结的版本
	addSite(t, d, g, "site-b")
	dep, err := d.Approve(req.ID, "看过了", "bob", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if dep.RevisionID != req.RevisionID || waitDone(t, d, dep).Status != "APPLIED" || currentRev(t, d, g) != req.RevisionID {
		t.Fatalf("approved deployment %+v", dep)
	}
	done, _ := d.GetRequest(req.ID)
	if done.Status != ApprovalApproved || done.ReviewedBy != "bob" || done.DeploymentID != dep.ID {
		t.Fatalf("request after approve %+v", done.ReleaseRequest)
	}
	// 已处理的请求不能再处理
	if _, err := d.Approve(req.ID, "", "carol", "127.0.0.1"); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("approve twice: %v", err)
	}
	// 站点 b 的改动需要新的请求
	req2, err := d.RequestRelease(g.ID, "上线 b", "alice", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if req2.BaseRevisionID != req.RevisionID {
		t.Fatalf("base of second request = %q", req2.BaseRevisionID)
	}
	if err := d.Reject(req2.ID, "先别上", "bob", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.GetRequest(req2.ID); r.Status != ApprovalRejected || r.ReviewComment != "先别上" {
		t.Fatalf("rejected request %+v", r.ReleaseRequest)
	}
	if currentRev(t, d, g) != req.RevisionID {
		t.Fatal("reject must not change the live revision")
	}
}

func TestApprovalStaleAfterRollback(t *testing.T) {
	d, g := newTestService(t)
	addSite(t, d, g, "site-a")
	dep1, err := d.Publish(g.ID, "v1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep1)
	addSite(t, d, g, "site-b")
	dep2, err := d.Publish(g.ID, "v2", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep2)

	if err := d.SetApproval(true, "admin", ""); err != nil {
		t.Fatal(err)
	}
	addSite(t, d, g, "site-c")
	req, err := d.RequestRelease(g.ID, "v3", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	// 回滚不需要审批（应急，且目标是已经发布过的版本）
	rb, err := d.Rollback(g.ID, dep1.RevisionID, "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, rb)
	// 线上版本已变化：批准会被拒绝并把请求标为过期，避免把审批时看到的差异之外的改动带上线
	if _, err := d.Approve(req.ID, "", "bob", ""); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("approve stale: %v", err)
	}
	if r, _ := d.GetRequest(req.ID); r.Status != ApprovalStale {
		t.Fatalf("status %s", r.Status)
	}
	// 过期后可以重新提交
	if _, err := d.RequestRelease(g.ID, "v3 again", "alice", ""); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalCancelAndNoChanges(t *testing.T) {
	d, g := newTestService(t)
	if err := d.SetApproval(true, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequestRelease(g.ID, "empty", "alice", ""); err != nil {
		t.Fatal(err) // 空组的第一次发布也有内容（基线配置）
	}
	reqs, _ := d.ListRequests(g.ID, "")
	if len(reqs) != 1 {
		t.Fatalf("list = %d", len(reqs))
	}
	if err := d.Cancel(reqs[0].ID, "bob", false, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("others cancel: %v", err)
	}
	if err := d.Cancel(reqs[0].ID, "alice", false, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.GetRequest(reqs[0].ID); r.Status != ApprovalCancelled {
		t.Fatalf("status %s", r.Status)
	}
	// 关闭审批后恢复直接发布
	if err := d.SetApproval(false, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if d.ApprovalEnabled() {
		t.Fatal("approval should be off")
	}
	dep, err := d.Publish(g.ID, "", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep)
	if err := d.SetApproval(true, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequestRelease(g.ID, "nothing", "alice", ""); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("no changes: %v", err)
	}
}
