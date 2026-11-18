package deploy

import (
	"context"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func siteRevision(t *testing.T) *store.Revision {
	t.Helper()
	b := &render.Bundle{Files: map[string][]byte{
		"conf.d/site-a.conf": []byte("server {\n    listen 80;\n    server_name a.example.com;\n}\n"),
		"conf.d/site-w.conf": []byte("server {\n    server_name *.w.example.com;\n}\n"),
	}}
	data, err := b.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	return &store.Revision{ID: "r000001", Bundle: data}
}

// 读取冻结版本失败必须报错，不能返回空列表（空列表表示“本来没有探活项”，会让业务探活被悄悄省略）。
func TestPostChecksReportsUnreadableRevision(t *testing.T) {
	d := &Service{}
	checks, err := d.postChecks(&store.Revision{ID: "r000001", Bundle: []byte("not a tar.gz")})
	if err == nil {
		t.Fatalf("unreadable revision must return an error, got checks=%v", checks)
	}
	if checks != nil {
		t.Fatalf("no checks may be returned together with an error: %v", checks)
	}
}

func TestPostChecksListsSites(t *testing.T) {
	d := &Service{}
	checks, err := d.postChecks(siteRevision(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0].Host != "a.example.com" || checks[1].Host != "probe.w.example.com" {
		t.Fatalf("unexpected checks: %v", checks)
	}
}

// 没有站点文件是合法的“无探活项”，与读取失败要能区分。
func TestPostChecksEmptyRevisionIsNotAnError(t *testing.T) {
	d := &Service{}
	data, err := (&render.Bundle{Files: map[string][]byte{"syedge.conf": []byte("events {}")}}).TarGz()
	if err != nil {
		t.Fatal(err)
	}
	checks, err := d.postChecks(&store.Revision{ID: "r000001", Bundle: data})
	if err != nil || len(checks) != 0 {
		t.Fatalf("checks=%v err=%v", checks, err)
	}
}

// recordAgents 记录收到的指令类型；全部回复成功。
type recordAgents struct {
	mu   sync.Mutex
	sent []string
}

func (a *recordAgents) Online(uint) bool { return true }
func (a *recordAgents) Send(_ context.Context, _ uint, m *agentv1.ServerMessage, _ time.Duration) (*agentv1.CommandResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case m.GetRollback() != nil:
		a.sent = append(a.sent, "rollback")
	case m.GetApply() != nil:
		a.sent = append(a.sent, "apply")
	}
	return &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}, nil
}

func brokenRevision() *store.Revision {
	return &store.Revision{ID: "r000009", Bundle: []byte("not a tar.gz")}
}

func runWithBrokenRevision(t *testing.T, kind string, withNode bool) (*Service, *store.NodeGroup, *recordAgents, *store.Deployment) {
	t.Helper()
	d, g := newTestService(t)
	ag := &recordAgents{}
	d.Agents = ag
	dep := &store.Deployment{GroupID: g.ID, RevisionID: "r000009", Kind: kind, Status: "APPLYING", CreatedBy: "tester", StartedAt: time.Now()}
	var nodes []store.Node
	if withNode {
		n := store.Node{GroupID: g.ID, Name: "edge", Status: "online", AppliedRevision: "r000001"}
		if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(&n); err != nil {
			t.Fatal(err)
		}
		nodes = []store.Node{n}
		dep.Nodes = []store.DeploymentNode{{NodeID: n.ID, NodeName: n.Name, Status: "PENDING"}}
	}
	if err := query.Use(d.Store.DB).Deployment.WithContext(context.Background()).Create(dep); err != nil {
		t.Fatal(err)
	}
	d.run(g, brokenRevision(), dep, nodes)
	got, err := query.Use(d.Store.DB).Deployment.WithContext(context.Background()).Where(query.Use(d.Store.DB).Deployment.ID.Eq(dep.ID)).First()
	if err != nil {
		t.Fatal(err)
	}
	return d, g, ag, got
}

// 没有任何节点时，读不出来的版本也不能被标成 APPLIED 并成为节点组当前版本。
func TestRunWithUnreadableRevisionAndNoNodesFails(t *testing.T) {
	d, g, _, dep := runWithBrokenRevision(t, "publish", false)
	if dep.Status != "FAILED" {
		t.Fatalf("status = %s, want FAILED", dep.Status)
	}
	cur, err := store.First[store.NodeGroup](d.Store, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.CurrentRevisionID != "" {
		t.Fatalf("unreadable revision became the group's current revision: %s", cur.CurrentRevisionID)
	}
}

// 发布一个读不出来的版本：节点不下发，标 FAILED。
func TestRunWithUnreadableRevisionFailsPublishToNodes(t *testing.T) {
	_, _, ag, dep := runWithBrokenRevision(t, "publish", true)
	if dep.Status != "FAILED" || len(ag.sent) != 0 {
		t.Fatalf("status=%s sent=%v", dep.Status, ag.sent)
	}
}

// 回滚到节点本地已有的版本不需要探活计划：控制台读不出版本包时仍要发出本地回滚指令。
func TestLocalRollbackDoesNotNeedPostCheckPlan(t *testing.T) {
	_, _, ag, dep := runWithBrokenRevision(t, "rollback", true)
	if len(ag.sent) != 1 || ag.sent[0] != "rollback" {
		t.Fatalf("sent=%v, want a single local rollback", ag.sent)
	}
	if dep.Status != "APPLIED" {
		t.Fatalf("status = %s, want APPLIED", dep.Status)
	}
}
