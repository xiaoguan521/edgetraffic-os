package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 实例档案接口：经真实路由、角色与授权中间件和数据库（设计 0001 第 5.2 节）。
func TestProfileHTTP(t *testing.T) {
	f := newImportHTTP(t)
	path := fmt.Sprintf("/node-groups/%d/profile", f.group)

	w := f.request("viewer", http.MethodGet, path, nil, "", false)
	requireStatus(t, w, http.StatusOK, "")
	var v profileView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Instance != "" || v.Effective.HTTPPort != 80 || v.Effective.StatusPort != 8081 {
		t.Fatalf("default profile view: %v %s", err, w.Body)
	}

	blue := map[string]any{"instance": "blue", "http_port": 18080, "https_port": 18443, "status_port": 8082}
	requireStatus(t, f.json(t, "viewer", http.MethodPut, path, blue), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"instance": "Bad"}), http.StatusBadRequest, "INVALID_PROFILE")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"http_port": 8080, "https_port": 8080}), http.StatusBadRequest, "INVALID_PROFILE")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, blue), http.StatusOK, "")

	w = f.request("viewer", http.MethodGet, path, nil, "", false)
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Instance != "blue" || v.Effective.HTTPSPort != 18443 {
		t.Fatalf("saved profile view: %v %s", err, w.Body)
	}

	// 组内有节点后不能改实例名；同机另一个组占用了端口时不能改到该端口
	q := query.Use(f.s.Store.DB)
	ctx := context.Background()
	other := &store.NodeGroup{Name: "default-site"}
	if err := q.NodeGroup.WithContext(ctx).Create(other); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{{GroupID: f.group, Name: "edge-1b", HostKey: "host-a"}, {GroupID: other.ID, Name: "edge-1", HostKey: "host-a"}} {
		if err := q.Node.WithContext(ctx).Create(n); err != nil {
			t.Fatal(err)
		}
	}
	green := map[string]any{"instance": "green", "http_port": 18080, "https_port": 18443, "status_port": 8082}
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, green), http.StatusConflict, "PROFILE_LOCKED")
	clash := map[string]any{"instance": "blue", "http_port": 18080, "https_port": 443, "status_port": 8082}
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, clash), http.StatusConflict, "HOST_CONFLICT")

	var n int64
	f.s.Store.DB.Table("audit_logs").Where("action = ?", "group.profile").Count(&n)
	if n != 1 {
		t.Fatalf("audit entries = %d, want 1 (only the successful change)", n)
	}
}

func TestEnrollCommand(t *testing.T) {
	def := enrollCommand(render.Profile{}, "c:9443", "tok", "fp")
	if def != "sudo syedge-agent enroll -server c:9443 -token tok -ca-fingerprint fp && sudo systemctl enable syedge-agent && sudo systemctl start syedge-agent" {
		t.Fatalf("default = %q", def)
	}
	inst := enrollCommand(render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}, "c:9443", "tok", "fp")
	if inst != "sudo syedge-instance add blue --http 18080 --https 18443 --status 8082 --console c:9443 --token tok --ca-fingerprint fp" {
		t.Fatalf("instance = %q", inst)
	}
	// user 模式：不用 sudo，命令带安装根目录
	ud := enrollCommand(render.Profile{Mode: "user", Root: "/home/ops/edge", HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8081}, "c:9443", "tok", "fp")
	if ud != "/home/ops/edge/bin/syedge-agent enroll -root /home/ops/edge -server c:9443 -token tok -ca-fingerprint fp && /home/ops/edge/bin/syedge-instance --root /home/ops/edge ensure" {
		t.Fatalf("user default = %q", ud)
	}
	ui := enrollCommand(render.Profile{Instance: "blue", Mode: "user", Root: "/home/ops/edge", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 18082}, "c:9443", "tok", "fp")
	if ui != "/home/ops/edge/bin/syedge-instance --root /home/ops/edge add blue --http 18080 --https 18443 --status 18082 --console c:9443 --token tok --ca-fingerprint fp" {
		t.Fatalf("user instance = %q", ui)
	}
	if strings.Contains(ud+ui, "sudo") {
		t.Fatal("user mode command uses sudo")
	}
}

func TestBrandEndpoint(t *testing.T) {
	f := newImportHTTP(t)
	w := f.request("", http.MethodGet, "/brand", nil, "", false) // 不需要登录
	requireStatus(t, w, http.StatusOK, "")
	var b map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"id": "syedge", "proc": "SyEdge", "product": "SyEdge", "fullname": "神玥企业级动态网关与负载均衡引擎", "agent": "syedge-agent", "lua": "syedge-lua", "instance": "syedge-instance"}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("%s = %q, want %q", k, b[k], v)
		}
	}
}
