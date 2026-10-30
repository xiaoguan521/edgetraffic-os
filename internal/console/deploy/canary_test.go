package deploy

import (
	"context"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
)

// applyAgents 模拟节点：收到 Apply / Rollback 即成功，并记录节点当前版本。
type applyAgents struct{ d *Service }

func (a *applyAgents) Online(uint) bool { return true }
func (a *applyAgents) Send(_ context.Context, id uint, m *agentv1.ServerMessage, _ time.Duration) (*agentv1.CommandResult, error) {
	return &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}, nil
}

func waitCanary(t *testing.T, d *Service, id uint) *CanaryView {
	t.Helper()
	for i := 0; i < 400; i++ {
		v, err := d.GetCanary(id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != "running" {
			return v
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("canary did not finish")
	return nil
}

func canaryFixture(t *testing.T) (*Service, *store.NodeGroup, []uint, string) {
	d, g := newTestService(t)
	addSite(t, d, g, "site-a")
	dep, err := d.Publish(g.ID, "v1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep)
	ids := addNodes(t, d, g, 10)
	aa := &applyAgents{d: d}
	d.Agents = aa
	addSite(t, d, g, "site-b")
	return d, g, ids, dep.RevisionID
}

func TestCanaryPromotes(t *testing.T) {
	d, g, ids, _ := canaryFixture(t)
	c, err := d.StartCanary(g.ID, "v2", "alice", "", CanaryOptions{Percents: []int{10, 50, 100}, Observe: 50 * time.Millisecond, Max5xxPercent: 5, MinRequests: 50})
	if err != nil {
		t.Fatal(err)
	}
	v := waitCanary(t, d, c.ID)
	if v.Status != "done" || len(v.Batches) != 3 || len(v.Batches[0].Nodes) != 1 || len(v.Batches[1].Nodes) != 4 || len(v.Batches[2].Nodes) != 5 {
		t.Fatalf("canary %+v", v)
	}
	if currentRev(t, d, g) != c.RevisionID {
		t.Fatal("group revision not promoted")
	}
	_ = ids
}

func TestCanaryRollsBackOn5xx(t *testing.T) {
	d, g, ids, base := canaryFixture(t)
	// 第一批（ids[0]）在观察期内 5xx 偏高
	q := query.Use(d.Store.DB)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = q.MetricMinute.WithContext(context.Background()).Create(&store.MetricMinute{NodeID: ids[0], Site: "site-a", Minute: time.Now().Unix(), Requests: 200, S5xx: 40})
	}()
	c, err := d.StartCanary(g.ID, "v2", "alice", "", CanaryOptions{Percents: []int{10, 100}, Observe: 150 * time.Millisecond, Max5xxPercent: 5, MinRequests: 50})
	if err != nil {
		t.Fatal(err)
	}
	v := waitCanary(t, d, c.ID)
	if v.Status != "rolled_back" || v.Batches[0].Rate < 19 || v.Batches[1].Status != "skipped" {
		t.Fatalf("canary %+v", v)
	}
	if currentRev(t, d, g) != base {
		t.Fatal("group revision must stay on the base revision")
	}
	// 灰度进行中不能普通发布（这里已结束，可以）
	dep, err := d.Publish(g.ID, "after", "alice", "")
	if err != nil {
		t.Fatalf("publish after rollback: %v", err)
	}
	waitDone(t, d, dep)
}
