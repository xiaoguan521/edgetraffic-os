package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
)

func TestBadgesAndAuditFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := storetest.Open(t)
	s := &Server{Store: st}

	st.DB.Create(&store.NodeGroup{Name: "default"})
	st.DB.Create(&store.Node{GroupID: 1, Name: "n1", Status: "online", Drifted: true})
	st.DB.Create(&store.Node{GroupID: 1, Name: "n2", Status: "offline"})
	st.DB.Create(&store.Snippet{GroupID: 1, Scope: "http", Name: "s1", PendingBy: "alice"})
	st.DB.Create(&store.AlertEvent{Kind: "node_offline", Status: "firing", Message: "offline"})
	st.DB.Create(&store.AuditLog{TS: time.Now(), Username: "alice", Action: "site.create", ObjectType: "site", ObjectID: "shop", Result: "success", Detail: "created"})
	st.DB.Create(&store.AuditLog{TS: time.Now(), Username: "bob", Action: "release.publish", ObjectType: "group", ObjectID: "1", Result: "failed", Detail: "oops"})

	r := gin.New()
	r.GET("/badges", s.badges)
	r.GET("/audit", s.listAudit)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/badges", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("badges status = %d", w.Code)
	}
	var b map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b["drifted_nodes"] != 1 || b["offline_nodes"] != 1 || b["pending_snippets"] != 1 || b["firing_alerts"] != 1 {
		t.Fatalf("unexpected badges: %+v", b)
	}

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/audit?username=alice&q=shop", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("audit status = %d", w2.Code)
	}
	var logs []store.AuditLog
	if err := json.Unmarshal(w2.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Username != "alice" {
		t.Fatalf("expected 1 filtered audit log, got %d", len(logs))
	}
}
