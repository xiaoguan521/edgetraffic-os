package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/ha"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

func haRouter(t *testing.T, el *ha.Elector) *gin.Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	s := &Server{Store: st, HA: el}
	r := gin.New()
	r.GET("/healthz", s.healthz)
	r.GET("/livez", s.livez)
	r.GET("/readyz", s.readyz)
	r.GET("/metrics", s.promMetrics)
	r.Use(s.standbyGuard)
	r.GET("/api/v1/x", func(c *gin.Context) { c.String(200, "ok") })
	return r
}

func get(r http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestStandbyRejectsAPIButAnswersHealthz(t *testing.T) {
	r := haRouter(t, &ha.Elector{}) // 未运行选主：不是主
	if w := get(r, "/api/v1/x"); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("standby api = %d %s", w.Code, w.Body)
	}
	if w := get(r, "/healthz"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("standby healthz = %d", w.Code)
	}
	// /livez 与 /readyz 在备机上仍返回 200（区分进程存活/实例就绪与 VIP 选主）
	if w := get(r, "/livez"); w.Code != http.StatusOK {
		t.Fatalf("standby livez = %d %s", w.Code, w.Body)
	}
	if w := get(r, "/readyz"); w.Code != http.StatusOK {
		t.Fatalf("standby readyz = %d %s", w.Code, w.Body)
	}
}

func TestPromMetricsAuthAndLoopback(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetKV("last_backup_at", time.Now().UTC().Format(time.RFC3339))
	grp := store.NodeGroup{Name: "prod"}
	st.DB.Create(&grp)
	nd := store.Node{GroupID: grp.ID, Name: "edge-1", Status: "online", CPUPercent: 12.5, DiskUsedPercent: 63.4, DiskInodePercent: 18.2, RequestsTotal: 42}
	st.DB.Create(&nd)
	st.DB.Create(&store.MetricMinute{
		NodeID: nd.ID, Site: "api.example.com", Minute: time.Now().Add(-1 * time.Minute).Truncate(time.Minute).Unix(),
		Requests: 120, S2xx: 110, S3xx: 2, S4xx: 5, S5xx: 3, P50ms: 12, P95ms: 85.5, Bytes: 4096,
	})

	s := &Server{Store: st, jwtKey: []byte("test-secret-key-32-bytes-minimum")}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", s.promMetrics)

	// 1. 非 loopback 且未携带凭证：拒绝（401）
	reqRemote := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqRemote.RemoteAddr = "192.0.2.10:54321"
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, reqRemote)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated remote /metrics = %d, want 401", w1.Code)
	}

	// 2. 本机 loopback 免认证采集：返回 200 与 Prometheus 指标
	reqLocal := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqLocal.RemoteAddr = "127.0.0.1:54321"
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, reqLocal)
	if w2.Code != http.StatusOK {
		t.Fatalf("loopback /metrics = %d %s", w2.Code, w2.Body.String())
	}
	body := w2.Body.String()
	for _, want := range []string{
		"etos_console_leader 1",
		"etos_console_db_up 1",
		`etos_nodes_total{status="online"} 1`,
		`etos_node_requests_total{node="edge-1"} 42`,
		`etos_node_disk_used_percent{node="edge-1"} 63.40`,
		`etos_node_disk_inode_percent{node="edge-1"} 18.20`,
		`etos_site_requests_5m{group="prod",site="api.example.com",status_class="2xx"} 110`,
		`etos_site_requests_5m{group="prod",site="api.example.com",status_class="5xx"} 3`,
		`etos_site_p95_latency_ms{group="prod",site="api.example.com"} 85.50`,
		`etos_site_bytes_sent_5m{group="prod",site="api.example.com"} 4096`,
		"etos_backup_last_success_timestamp_seconds",
	} {
		if !containsLine(body, want) {
			t.Fatalf("missing %q in metrics output:\n%s", want, body)
		}
	}

	// 3. 远程携带有效 Bearer API Token：返回 200
	rawTok := "etos_test_metrics_token_123456"
	st.DB.Create(&store.User{Username: "ops", Role: store.RoleViewer})
	st.DB.Create(&store.APIToken{
		Name:      "prom",
		Username:  "ops",
		Role:      store.RoleViewer,
		TokenHash: hashToken(rawTok),
		Prefix:    rawTok[:12],
	})
	reqAuth := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqAuth.RemoteAddr = "192.0.2.10:54321"
	reqAuth.Header.Set("Authorization", "Bearer "+rawTok)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, reqAuth)
	if w3.Code != http.StatusOK {
		t.Fatalf("authenticated remote /metrics = %d %s", w3.Code, w3.Body.String())
	}
}

func containsLine(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || filepath.Base(s) != "" && (stringContains(s, sub)))
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestLeaderServes(t *testing.T) {
	el := &ha.Elector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go el.Run(ctx, func(context.Context) {}, nil)
	for i := 0; !el.IsLeader(); i++ {
		if i > 100 {
			t.Fatal("never became leader")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r := haRouter(t, el)
	if w := get(r, "/api/v1/x"); w.Code != 200 {
		t.Fatalf("leader api = %d", w.Code)
	}
	if w := get(r, "/healthz"); w.Code != 200 {
		t.Fatalf("leader healthz = %d %s", w.Code, w.Body)
	}
}
