package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func testCloneServer(t *testing.T) (*Server, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := storetest.Open(t)
	s := &Server{
		Store:  st,
		Deploy: &deploy.Service{Store: st},
		Lic:    &licsvc.Service{Store: st},
		Audit:  &audit.Logger{DB: st.DB},
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user", "alice")
		c.Next()
	})
	r.POST("/node-groups/:id/copy-from", s.copyGroupConfig)
	r.GET("/ip-sets", s.listIPSets)
	r.POST("/ip-sets", s.createIPSet)
	r.DELETE("/ip-sets/:id", s.deleteIPSet)
	r.GET("/rule-templates", s.listRuleTemplates)
	r.POST("/rule-templates", s.saveRuleTemplate)
	r.DELETE("/rule-templates/:name", s.deleteRuleTemplate)
	return s, r
}

func doJSON(r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGlobalIPSetOverrideAndUsageGuard(t *testing.T) {
	s, r := testCloneServer(t)
	s.Store.DB.Create(&store.NodeGroup{Name: "east"})
	s.Store.DB.Create(&store.NodeGroup{Name: "west"})

	// 创建全局 IP 集（group_id = 0）
	w := doJSON(r, http.MethodPost, "/ip-sets", map[string]any{
		"group_id": 0, "name": "corp-office", "label": "总部出口", "cidrs": []string{"10.10.0.0/16"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create global ip set failed: %d %s", w.Code, w.Body.String())
	}
	var created ipSetView
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if !created.Global || created.GroupID != 0 {
		t.Fatalf("expected global=true, got %+v", created)
	}

	// 在 east 组创建同名覆盖 IP 集
	w2 := doJSON(r, http.MethodPost, "/ip-sets", map[string]any{
		"group_id": 1, "name": "corp-office", "label": "华东覆盖", "cidrs": []string{"10.20.0.0/16"},
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("create override ip set failed: %d %s", w2.Code, w2.Body.String())
	}

	// 验证 deploy.IPSets 合并逻辑：east 用 10.20.0.0/16，west 用全局 10.10.0.0/16
	eastSets, err := s.Deploy.IPSets(1)
	if err != nil || len(eastSets) != 1 || eastSets[0].CIDRs[0] != "10.20.0.0/16" {
		t.Fatalf("east sets = %+v, err=%v", eastSets, err)
	}
	westSets, err := s.Deploy.IPSets(2)
	if err != nil || len(westSets) != 1 || westSets[0].CIDRs[0] != "10.10.0.0/16" {
		t.Fatalf("west sets = %+v, err=%v", westSets, err)
	}

	// 在 west 组站点引用 corp-office，全局 IP 集应禁止删除
	specBytes, _ := json.Marshal(render.Site{
		Name:    "app",
		Domains: []string{"app.example.com"},
		Routes:  []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}},
		Rules:   []render.Rule{{Name: "allow-corp", Conditions: []render.Condition{{Kind: "ip_set", Set: "corp-office"}}, Action: render.RuleAction{Type: "deny", Status: 403}}},
	})
	s.Store.DB.Create(&store.Site{GroupID: 2, Name: "app", Spec: string(specBytes), Enabled: true, Version: 1})

	wDel := doJSON(r, http.MethodDelete, "/ip-sets/1", nil)
	if wDel.Code != http.StatusConflict {
		t.Fatalf("expected 409 when deleting global ip set in use, got %d: %s", wDel.Code, wDel.Body.String())
	}

	// 验证 listIPSets 批量聚合 Users（无 N+1）：west 引用全局 corp-office，east 覆盖不计入全局
	wList := doJSON(r, http.MethodGet, "/ip-sets?group_id=2", nil)
	if wList.Code != http.StatusOK {
		t.Fatalf("list ip sets failed: %d %s", wList.Code, wList.Body.String())
	}
	var list []ipSetView
	_ = json.Unmarshal(wList.Body.Bytes(), &list)
	if len(list) != 1 || len(list[0].Users) != 1 || list[0].Users[0] != "west/app/allow-corp" {
		t.Fatalf("unexpected batch ipset users: %+v", list)
	}
}

func TestCopyGroupConfigAndRuleTemplates(t *testing.T) {
	s, r := testCloneServer(t)
	s.Store.DB.Create(&store.NodeGroup{Name: "src", AllowedCIDRs: "10.1.0.0/16"})
	s.Store.DB.Create(&store.NodeGroup{Name: "dst", AllowedCIDRs: "10.2.0.0/16"})

	upSpec, _ := json.Marshal(render.UpstreamGroup{Name: "api", Algorithm: "least_conn", Servers: []render.UpstreamServer{{Addr: "10.1.1.10:8080"}}})
	s.Store.DB.Create(&store.Upstream{GroupID: 1, Name: "api", Spec: string(upSpec)})
	s.Store.DB.Create(&store.IPSet{GroupID: 1, Name: "allowlist", Label: "白名单", Source: "manual", CIDRs: `["192.168.1.0/24"]`, Count: 1})
	sb := store.StaticBundle{GroupID: 1, Name: "fe.zip", SHA256: "abc123", Size: 100, Files: 2, Data: []byte("tgz")}
	s.Store.DB.Create(&sb)

	siteSpec, _ := json.Marshal(render.Site{
		Name:    "shop",
		Domains: []string{"shop.example.com"},
		Routes: []render.Route{
			{Path: "/api/", Match: "prefix", Mode: "proxy", Proxy: &render.ProxySpec{Upstream: "api"}},
			{Path: "/", Match: "prefix", Mode: "static", Static: &render.StaticSpec{BundleID: sb.ID}},
		},
		Rules: []render.Rule{{Name: "r1", Conditions: []render.Condition{{Kind: "ip_set", Set: "allowlist"}}, Action: render.RuleAction{Type: "deny", Status: 403}}},
	})
	siteRow := store.Site{GroupID: 1, Name: "shop", Spec: string(siteSpec), Enabled: true, Version: 1}
	s.Store.DB.Create(&siteRow)

	// 按选中站点复制：应自动带出引用的上游、组内 IP 集与静态资源包，并提醒回源网段不匹配
	w := doJSON(r, http.MethodPost, "/node-groups/2/copy-from", map[string]any{
		"source_group_id": 1,
		"site_ids":        []uint{siteRow.ID},
		"overwrite":       false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("copy-from failed: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Upstreams copyCount `json:"upstreams"`
		IPSets    copyCount `json:"ip_sets"`
		Sites     copyCount `json:"sites"`
		Warnings  []string  `json:"warnings"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Upstreams.Created != 1 || resp.IPSets.Created != 1 || resp.Sites.Created != 1 {
		t.Fatalf("unexpected copy counts: %+v", resp)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected allowed_cidrs mismatch warning")
	}

	// 验证目标组静态包已被独立复制并重绑
	var dstSite store.Site
	s.Store.DB.Where("group_id = ? AND name = ?", 2, "shop").First(&dstSite)
	var dstSpec render.Site
	_ = json.Unmarshal([]byte(dstSite.Spec), &dstSpec)
	if dstSpec.Routes[1].Static.BundleID == 0 || dstSpec.Routes[1].Static.BundleID == sb.ID {
		t.Fatalf("expected cloned static bundle id in target group, got %d (src %d)", dstSpec.Routes[1].Static.BundleID, sb.ID)
	}

	// 测试公共规则模板（含 JWT 密钥剥离校验）
	wt := doJSON(r, http.MethodPost, "/rule-templates", map[string]any{
		"name":        "api-protect",
		"description": "通用 API 限速与保护",
		"rules": []render.Rule{
			{Name: "jwt-check", Conditions: []render.Condition{{Kind: "path", Op: "prefix", Value: "/api/"}}, Action: render.RuleAction{Type: "jwt", JWT: &render.JWTAction{Alg: "HS256", Source: "header", Secret: "should-be-stripped"}}},
		},
	})
	if wt.Code != http.StatusOK {
		t.Fatalf("save rule template failed: %d %s", wt.Code, wt.Body.String())
	}
	var saved RuleTemplate
	_ = json.Unmarshal(wt.Body.Bytes(), &saved)
	if saved.Rules[0].Action.JWT.Secret != "" || saved.Rules[0].Action.JWT.SecretEnc != "" {
		t.Fatalf("JWT secret must be stripped in rule templates")
	}

	wl := doJSON(r, http.MethodGet, "/rule-templates", nil)
	if wl.Code != http.StatusOK {
		t.Fatalf("list rule templates failed: %d", wl.Code)
	}
	// 非法正则和不支持的拒绝状态码应被拦截
	wBadTpl := doJSON(r, http.MethodPost, "/rule-templates", map[string]any{
		"name": "bad-regex",
		"rules": []render.Rule{
			{Name: "r1", Conditions: []render.Condition{{Kind: "path", Op: "regex", Value: "[unclosed"}}, Action: render.RuleAction{Type: "deny", Status: 403}},
		},
	})
	if wBadTpl.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid template regex, got %d %s", wBadTpl.Code, wBadTpl.Body.String())
	}

	// 跨组复制站点域名冲突应产生 warning
	conflictSpec, _ := json.Marshal(render.Site{
		Name:    "other-shop",
		Domains: []string{"shop.example.com"},
		Routes:  []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}},
	})
	s.Store.DB.Create(&store.Site{GroupID: 1, Name: "other-shop", Spec: string(conflictSpec), Enabled: true, Version: 1})
	wConflict := doJSON(r, http.MethodPost, "/node-groups/2/copy-from", map[string]any{
		"source_group_id": 1,
		"include_sites":   true,
		"overwrite":       false,
	})
	if wConflict.Code != http.StatusOK {
		t.Fatalf("copy-from conflict test failed: %d %s", wConflict.Code, wConflict.Body.String())
	}
	var cresp struct {
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(wConflict.Body.Bytes(), &cresp)
	foundDomainWarn := false
	for _, msg := range cresp.Warnings {
		if bytes.Contains([]byte(msg), []byte("shop.example.com")) {
			foundDomainWarn = true
		}
	}
	if !foundDomainWarn {
		t.Fatalf("expected domain conflict warning, got %+v", cresp.Warnings)
	}

	wd := doJSON(r, http.MethodDelete, "/rule-templates/api-protect", nil)
	if wd.Code != http.StatusOK {
		t.Fatalf("delete rule template failed: %d", wd.Code)
	}
}
