package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestNodeUpdateMigrationAndGlobalSearch(t *testing.T) {
	s, r := testCloneServer(t)
	r.PATCH("/nodes/:id", s.updateNode)
	r.GET("/search", s.globalSearch)

	s.Store.DB.Create(&store.NodeGroup{Name: "g1"})
	s.Store.DB.Create(&store.NodeGroup{Name: "g2"})
	s.Store.DB.Create(&store.NodeGroup{Name: "g3"})
	s.Deploy.Audit = s.Audit
	if err := s.Deploy.SetProfile(3, render.Profile{HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 18081}, "alice", "127.0.0.1"); err != nil {
		t.Fatalf("SetProfile failed: %v", err)
	}
	nd := store.Node{GroupID: 1, Name: "ecs-old", RemoteAddr: "10.0.1.9:5555", Status: "online", AppliedRevision: "r000001"}
	s.Store.DB.Create(&nd)

	// 改名并迁移到档案一致的 g2 应成功
	w := doJSON(r, http.MethodPatch, "/nodes/1", map[string]any{"name": "edge-sh-01", "group_id": 2})
	if w.Code != http.StatusOK {
		t.Fatalf("rename+move failed: %d %s", w.Code, w.Body.String())
	}
	var updated store.Node
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Name != "edge-sh-01" || updated.GroupID != 2 {
		t.Fatalf("unexpected updated node: %+v", updated)
	}

	// 迁移到端口档案不一致的 g3 应返回 409 PROFILE_MISMATCH
	wBad := doJSON(r, http.MethodPatch, "/nodes/1", map[string]any{"group_id": 3})
	if wBad.Code != http.StatusConflict {
		t.Fatalf("expected 409 for profile mismatch, got %d %s", wBad.Code, wBad.Body.String())
	}

	// 全局跨组检索：同时建站点与上游
	upSpec, _ := json.Marshal(render.UpstreamGroup{Name: "pay-backend", Servers: []render.UpstreamServer{{Addr: "10.9.8.7:8080"}}})
	s.Store.DB.Create(&store.Upstream{GroupID: 2, Name: "pay-backend", Spec: string(upSpec)})
	siteSpec, _ := json.Marshal(render.Site{Name: "pay", Domains: []string{"pay.example.com"}, Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}})
	s.Store.DB.Create(&store.Site{GroupID: 2, Name: "pay", Spec: string(siteSpec), Enabled: true, Version: 1})

	ws := doJSON(r, http.MethodGet, "/search?q=10.9.8.7", nil)
	if ws.Code != http.StatusOK {
		t.Fatalf("search failed: %d %s", ws.Code, ws.Body.String())
	}
	var sr struct {
		Hits []searchHit `json:"hits"`
	}
	_ = json.Unmarshal(ws.Body.Bytes(), &sr)
	if len(sr.Hits) != 1 || sr.Hits[0].Kind != "upstream" || sr.Hits[0].GroupName != "g2" {
		t.Fatalf("unexpected search hits: %+v", sr.Hits)
	}

	// 组锁占用（发布中）时禁止跨组迁移，应返回 409 BUSY
	unlock, ok := s.Deploy.TryLockGroups(2)
	if !ok {
		t.Fatal("failed to acquire test group lock")
	}
	wBusy := doJSON(r, http.MethodPatch, "/nodes/1", map[string]any{"group_id": 1})
	unlock()
	if wBusy.Code != http.StatusConflict {
		t.Fatalf("expected 409 BUSY when group is locked, got %d %s", wBusy.Code, wBusy.Body.String())
	}

	// 观察接入模式下调用 SyncNode 应被拒绝，且不得产生虚假的 node.sync 审计记录
	s.Store.DB.Model(&store.NodeGroup{}).Where("id = ?", 2).Update("current_revision_id", "r000002")
	if err := s.Deploy.SetManagementMode(2, "observe", "alice", "127.0.0.1"); err != nil {
		t.Fatalf("SetManagementMode observe failed: %v", err)
	}
	if _, err := s.Deploy.SyncNode(1, "alice", true, "127.0.0.1"); err == nil {
		t.Fatalf("expected SyncNode to fail in observe mode")
	}
	var syncAudits int64
	s.Store.DB.Model(&store.AuditLog{}).Where("action = ?", "node.sync").Count(&syncAudits)
	if syncAudits != 0 {
		t.Fatalf("expected 0 node.sync audits on rejected observe sync, got %d", syncAudits)
	}
}
