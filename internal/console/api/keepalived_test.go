package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
)

func TestNormalizeVIPsAndHeldVIPs(t *testing.T) {
	got, err := normalizeVIPs(" 10.0.0.100 , 10.0.0.100 , 10.0.0.101 ")
	if err != nil || got != "10.0.0.100,10.0.0.101" {
		t.Fatalf("normalizeVIPs = %q, %v", got, err)
	}
	if _, err := normalizeVIPs("127.0.0.1"); err == nil {
		t.Fatal("loopback VIP should be rejected")
	}
	if _, err := normalizeVIPs("not-an-ip"); err == nil {
		t.Fatal("invalid VIP should be rejected")
	}
	held := heldVIPs("10.0.0.11,10.0.0.100", "10.0.0.100,10.0.0.101")
	if len(held) != 1 || held[0] != "10.0.0.100" {
		t.Fatalf("heldVIPs = %+v", held)
	}
}

func TestKeepalivedRenderAndNodeVIPRole(t *testing.T) {
	st := storetest.Open(t)
	grp := store.NodeGroup{
		Name:            "edge-ha",
		AllowedCIDRs:    "10.0.0.0/8",
		VIPs:            "10.0.0.100",
		InstanceProfile: `{"mode":"user","root":"/home/ops/syedge","http_port":8080,"https_port":8443,"status_port":18081}`,
	}
	if err := st.DB.Create(&grp).Error; err != nil {
		t.Fatal(err)
	}
	st.DB.Create(&store.Node{GroupID: grp.ID, Name: "edge-1", Status: "online", LocalIPs: "10.0.0.11,10.0.0.100"})
	st.DB.Create(&store.Node{GroupID: grp.ID, Name: "edge-2", Status: "online", LocalIPs: "10.0.0.12"})

	s := &Server{Store: st, Deploy: &deploy.Service{Store: st}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/nodes", s.listNodes)
	r.POST("/api/v1/keepalived/render", s.renderKeepalived)

	// 1. 节点列表自动标注 VIP 主 / VIP 备
	wNodes := httptest.NewRecorder()
	r.ServeHTTP(wNodes, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if wNodes.Code != http.StatusOK {
		t.Fatalf("listNodes = %d %s", wNodes.Code, wNodes.Body.String())
	}
	var listed []struct {
		Name     string   `json:"name"`
		VIPRole  string   `json:"vip_role"`
		HeldVIPs []string `json:"held_vips"`
	}
	if err := json.Unmarshal(wNodes.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].VIPRole != "master" || len(listed[0].HeldVIPs) != 1 || listed[1].VIPRole != "backup" {
		t.Fatalf("unexpected node VIP roles: %+v", listed)
	}

	// 2. 边缘节点模式生成 Keepalived 单播配置与 18081 状态页探活脚本
	payload := map[string]any{
		"mode":      "edge",
		"group_id":  grp.ID,
		"vip":       "10.0.0.100",
		"interface": "ens192",
		"vrid":      52,
		"auth_pass": "ha2026",
		"nodes": []map[string]any{
			{"name": "edge-1", "ip": "10.0.0.11", "priority": 100},
			{"name": "edge-2", "ip": "10.0.0.12", "priority": 90},
		},
	}
	body, _ := json.Marshal(payload)
	wEdge := httptest.NewRecorder()
	r.ServeHTTP(wEdge, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(body)))
	if wEdge.Code != http.StatusOK {
		t.Fatalf("render edge = %d %s", wEdge.Code, wEdge.Body.String())
	}
	var edgeResp keepalivedRenderResp
	if err := json.Unmarshal(wEdge.Body.Bytes(), &edgeResp); err != nil {
		t.Fatal(err)
	}
	if len(edgeResp.Configs) != 2 || !strings.Contains(edgeResp.CheckScript, "127.0.0.1:18081") {
		t.Fatalf("unexpected edge keepalived response: %+v", edgeResp)
	}
	if !strings.Contains(edgeResp.Configs[0].Config, "unicast_src_ip 10.0.0.11") || !strings.Contains(edgeResp.Configs[0].Config, "10.0.0.12") || !strings.Contains(edgeResp.Configs[0].Config, "10.0.0.100/24") {
		t.Fatalf("unexpected edge-1 config:\n%s", edgeResp.Configs[0].Config)
	}

	// 3. 控制台主备模式生成 /healthz 探活脚本
	payload["mode"] = "console"
	payload["console_url"] = "https://127.0.0.1:8443/healthz"
	body2, _ := json.Marshal(payload)
	wCon := httptest.NewRecorder()
	r.ServeHTTP(wCon, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(body2)))
	if wCon.Code != http.StatusOK {
		t.Fatalf("render console = %d %s", wCon.Code, wCon.Body.String())
	}
	var conResp keepalivedRenderResp
	_ = json.Unmarshal(wCon.Body.Bytes(), &conResp)
	if !strings.Contains(conResp.CheckScript, "https://127.0.0.1:8443/healthz") {
		t.Fatalf("unexpected console check script:\n%s", conResp.CheckScript)
	}

	// 4. 拒绝非法网卡名注入
	payload["interface"] = "eth0; rm -rf /"
	bodyBad, _ := json.Marshal(payload)
	wBad := httptest.NewRecorder()
	r.ServeHTTP(wBad, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(bodyBad)))
	if wBad.Code != http.StatusBadRequest {
		t.Fatalf("injection interface should be rejected with 400, got %d", wBad.Code)
	}

	// 5. 具名实例使用隔离的脚本路径与 VRRP 实例名
	namedGrp := store.NodeGroup{
		Name:            "edge-blue",
		AllowedCIDRs:    "10.0.0.0/8",
		InstanceProfile: `{"instance":"blue","mode":"system","http_port":8080,"https_port":8443,"status_port":18082}`,
	}
	st.DB.Create(&namedGrp)
	namedPayload := map[string]any{
		"mode": "edge", "group_id": namedGrp.ID, "vip": "10.0.0.200", "interface": "eth0", "vrid": 53,
		"nodes": []map[string]any{
			{"name": "blue-1", "ip": "10.0.0.21", "priority": 100},
			{"name": "blue-2", "ip": "10.0.0.22", "priority": 90},
		},
	}
	bNamed, _ := json.Marshal(namedPayload)
	wNamed := httptest.NewRecorder()
	r.ServeHTTP(wNamed, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(bNamed)))
	if wNamed.Code != http.StatusOK {
		t.Fatalf("named instance render = %d %s", wNamed.Code, wNamed.Body.String())
	}
	var namedResp keepalivedRenderResp
	_ = json.Unmarshal(wNamed.Body.Bytes(), &namedResp)
	if !strings.HasSuffix(namedResp.CheckScriptPath, "-edge-blue.sh") || !strings.Contains(namedResp.Configs[0].Config, "_EDGE_BLUE") {
		t.Fatalf("named instance should use isolated script/tag: %+v", namedResp)
	}

	// 6. IPv6 VRRPv3 不输出 authentication 块；地址族混填、节点 IP 等于 VIP、节点名含换行均拒绝
	v6Payload := map[string]any{
		"mode": "edge", "vip": "2001:db8::100", "interface": "eth0", "vrid": 54,
		"nodes": []map[string]any{
			{"name": "v6-1", "ip": "2001:db8::11"},
			{"name": "v6-2", "ip": "2001:db8::12"},
		},
	}
	bV6, _ := json.Marshal(v6Payload)
	wV6 := httptest.NewRecorder()
	r.ServeHTTP(wV6, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(bV6)))
	if wV6.Code != http.StatusOK {
		t.Fatalf("ipv6 render = %d %s", wV6.Code, wV6.Body.String())
	}
	var v6Resp keepalivedRenderResp
	_ = json.Unmarshal(wV6.Body.Bytes(), &v6Resp)
	if strings.Contains(v6Resp.Configs[0].Config, "auth_type PASS") || !strings.Contains(v6Resp.Configs[0].Config, "2001:db8::100/64") {
		t.Fatalf("unexpected ipv6 config:\n%s", v6Resp.Configs[0].Config)
	}

	for _, bad := range []map[string]any{
		// IPv4 VIP + IPv6 node
		{"mode": "edge", "vip": "10.0.0.100", "nodes": []map[string]any{{"name": "a", "ip": "10.0.0.11"}, {"name": "b", "ip": "2001:db8::12"}}},
		// Node IP equals VIP
		{"mode": "edge", "vip": "10.0.0.100", "nodes": []map[string]any{{"name": "a", "ip": "10.0.0.100"}, {"name": "b", "ip": "10.0.0.12"}}},
		// Node name with newline injection
		{"mode": "edge", "vip": "10.0.0.100", "nodes": []map[string]any{{"name": "a\nglobal_defs{}", "ip": "10.0.0.11"}, {"name": "b", "ip": "10.0.0.12"}}},
	} {
		bb, _ := json.Marshal(bad)
		ww := httptest.NewRecorder()
		r.ServeHTTP(ww, httptest.NewRequest(http.MethodPost, "/api/v1/keepalived/render", bytes.NewReader(bb)))
		if ww.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad payload %+v, got %d %s", bad, ww.Code, ww.Body.String())
		}
	}
}
