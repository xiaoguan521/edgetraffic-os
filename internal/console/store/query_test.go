package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
)

// 生成的查询在 SQLite 与 PostgreSQL 上都可用（类型安全的条件、排序、事务、预加载关联）。
func TestGeneratedQueries(t *testing.T) {
	st := storetest.Open(t)
	q := query.Use(st.DB)
	ctx := context.Background()
	g := &store.NodeGroup{Name: "default"}
	if err := q.NodeGroup.WithContext(ctx).Create(g); err != nil {
		t.Fatal(err)
	}
	if err := q.Transaction(func(tx *query.Query) error {
		for _, id := range []string{"r000001", "r000002"} {
			if err := tx.Revision.WithContext(ctx).Create(&store.Revision{ID: id, GroupID: g.ID}); err != nil {
				return err
			}
		}
		return tx.Deployment.WithContext(ctx).Create(&store.Deployment{GroupID: g.ID, RevisionID: "r000002", Kind: "publish", Status: "APPLIED",
			StartedAt: time.Now(), Nodes: []store.DeploymentNode{{NodeID: 1, NodeName: "edge-1", Status: "APPLIED"}}})
	}); err != nil {
		t.Fatal(err)
	}
	r := q.Revision
	revs, err := r.WithContext(ctx).Where(r.GroupID.Eq(g.ID)).Order(r.Seq.Desc()).Find()
	if err != nil || len(revs) != 2 || revs[0].ID != "r000002" {
		t.Fatalf("revisions: %+v %v", revs, err)
	}
	// 用户输入作为参数绑定，不会成为 SQL 的一部分
	n, err := q.NodeGroup.WithContext(ctx).Where(q.NodeGroup.Name.Eq("default' OR '1'='1")).Count()
	if err != nil || n != 0 {
		t.Fatalf("injection-shaped value matched: %d %v", n, err)
	}
	d := q.Deployment
	dep, err := d.WithContext(ctx).Preload(d.Nodes).Where(d.RevisionID.Eq("r000002")).First()
	if err != nil || len(dep.Nodes) != 1 || dep.Nodes[0].NodeName != "edge-1" {
		t.Fatalf("deployment with nodes: %+v %v", dep, err)
	}
	if _, err := q.User.WithContext(ctx).Where(q.User.Username.Eq("nobody")).First(); err == nil {
		t.Fatal("expected not found")
	}
}
