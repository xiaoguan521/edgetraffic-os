package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 发布一次，模拟节点上手改配置后的漂移上报，再采纳为片段。
func TestAdoptDrift(t *testing.T) {
	d, g := newTestService(t)
	addProxySite(t, d, g)
	dep, err := d.Publish(g.ID, "v1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep)
	rev, _ := d.revision(dep.RevisionID, g.ID)
	files, _ := render.ReadTarGz(rev.Bundle)
	site := string(files["conf.d/site-shop.conf"])

	// 节点上的手工改动：server 层加一条、/api/ 路由里加一条、再加一个新的 location；另外改了全局配置
	mod := strings.Replace(site, "    include  snippets/security.conf;", "    include  snippets/security.conf;\n    client_max_body_size 30m;", 1)
	mod = strings.Replace(mod, "        proxy_pass        http://up_api;", "        proxy_pass        http://up_api;\n        proxy_read_timeout 300s;", 1)
	mod = strings.Replace(mod, "\n}\n", "\n    location /health { return 200; }\n}\n", 1)
	if mod == site {
		t.Fatal("test setup: replacements did not apply")
	}
	q := query.Use(d.Store.DB)
	ctx := context.Background()
	node := &store.Node{GroupID: g.ID, Name: "edge-1", Status: "online", Drifted: true, AppliedRevision: dep.RevisionID}
	_ = q.Node.WithContext(ctx).Create(node)
	fj, _ := json.Marshal([]gateway.DriftFileView{
		{Path: "conf.d/site-shop.conf", Change: "modified", Content: mod},
		{Path: "syapp.conf", Change: "modified", Content: "worker_processes 64;"},
		{Path: "certs/x.key", Change: "added", Omitted: true},
	})
	_ = q.NodeDrift.WithContext(ctx).Create(&store.NodeDrift{NodeID: node.ID, RevisionID: dep.RevisionID, Files: string(fj)})

	view, err := d.DriftView(node.ID)
	if err != nil || len(view.Files) != 3 || !strings.Contains(view.Files[0].Diff, "+    client_max_body_size 30m;") {
		t.Fatalf("drift view: %v %+v", err, view)
	}
	res, err := d.AdoptDrift(node.ID, "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) != 2 {
		t.Fatalf("expected server + location snippets, got %+v", res)
	}
	if len(res.Rejected) != 2 {
		t.Fatalf("syapp.conf and the key file cannot be adopted: %+v", res.Rejected)
	}
	sns, _ := d.ListSnippets(g.ID)
	var server, loc *store.Snippet
	for _, s := range sns {
		switch s.Scope {
		case "server":
			server = s
		case "location":
			loc = s
		}
	}
	if server == nil || !strings.Contains(server.Pending, "client_max_body_size  30m;") || !strings.Contains(server.Pending, "location  /health") ||
		server.Origin != "drift" || server.PendingBy != "bob" || !strings.Contains(server.PendingNote, "edge-1") {
		t.Fatalf("server snippet %+v", server)
	}
	if loc == nil || loc.RoutePath != "/api/" || !strings.Contains(loc.Pending, "proxy_read_timeout  300s;") {
		t.Fatalf("location snippet %+v", loc)
	}
	// 采纳人不能自己批准（另一人批准后随下一次发布生效）
	if err := d.ApproveSnippet(server.ID, "bob", ""); err != ErrSelfApproval {
		t.Fatalf("self approve: %v", err)
	}
}

func TestAdoptDriftRejectsRemovals(t *testing.T) {
	d, g := newTestService(t)
	addProxySite(t, d, g)
	dep, _ := d.Publish(g.ID, "v1", "alice", "")
	waitDone(t, d, dep)
	rev, _ := d.revision(dep.RevisionID, g.ID)
	files, _ := render.ReadTarGz(rev.Bundle)
	// 删掉渲染器生成的安全基线 include：不能采纳，只能恢复
	mod := strings.Replace(string(files["conf.d/site-shop.conf"]), "    include  snippets/security.conf;\n", "", 1)
	q := query.Use(d.Store.DB)
	ctx := context.Background()
	node := &store.Node{GroupID: g.ID, Name: "edge-2", Drifted: true, AppliedRevision: dep.RevisionID}
	_ = q.Node.WithContext(ctx).Create(node)
	fj, _ := json.Marshal([]gateway.DriftFileView{{Path: "conf.d/site-shop.conf", Change: "modified", Content: mod}})
	_ = q.NodeDrift.WithContext(ctx).Create(&store.NodeDrift{NodeID: node.ID, RevisionID: dep.RevisionID, Files: string(fj)})
	res, err := d.AdoptDrift(node.ID, "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) != 0 || len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0], "include") {
		t.Fatalf("%+v", res)
	}
}
