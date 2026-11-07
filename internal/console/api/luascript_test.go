package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 自定义 Lua 脚本：只有管理员能提交与审批（批准人 ≠ 提交人），需要 lua 授权，批准后才进入渲染。
func TestLuaScriptSnippetHTTP(t *testing.T) {
	f := newImportHTTP(t)
	q := query.Use(f.s.Store.DB)
	ctx := context.Background()
	lua := false
	f.s.Deploy.Feature = func(name string) bool { return name != "lua" || lua }

	// 第二位管理员（审批人）
	hash, err := crypto.HashPassword("Import@2026!test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := q.User.WithContext(ctx).Create(&store.User{Username: "admin2", Role: store.RoleAdmin, Source: "local", PasswordHash: hash, PasswordChangedAt: &now}); err != nil {
		t.Fatal(err)
	}
	w := f.json(t, "", http.MethodPost, "/auth/login", map[string]string{"username": "admin2", "password": "Import@2026!test"})
	requireStatus(t, w, http.StatusOK, "")
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			f.cookies["admin2"] = c
		}
	}

	spec, _ := json.Marshal(render.Site{Name: "test", Domains: []string{"test.example.com"}, Routes: []render.Route{
		{Path: "/", Match: "prefix", Mode: "static", Static: &render.StaticSpec{IndexHTML: "<h1>hi</h1>"}},
		{Path: "/lua", Match: "exact", Mode: "static", Static: &render.StaticSpec{IndexHTML: "x"}},
		{Path: "/old", Match: "exact", Mode: "redirect", Redirect: &render.RedirectSpec{To: "https://example.com/", Code: 301}},
	}})
	site := &store.Site{GroupID: f.group, Name: "test", Spec: string(spec), Enabled: true}
	if err := q.Site.WithContext(ctx).Create(site); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"group_id": f.group, "site_id": site.ID, "scope": "lua", "route_path": "/lua", "name": "content",
		"content": `ngx.say("<h1>Hello from Lua</h1>")`}

	// 操作员不能提交、校验 Lua 脚本；没有 lua 授权时管理员也不能
	requireStatus(t, f.json(t, "operator", http.MethodPost, "/snippets", body), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "operator", http.MethodPost, "/snippets/validate", body), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/snippets", body), http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED")
	lua = true

	// 不合法：未知阶段、跳转路由上的 content 阶段、不存在的路由
	bad := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range body {
			m[a] = b
		}
		m[k] = v
		return m
	}
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/snippets", bad("name", "init")), http.StatusBadRequest, "SNIPPET_INVALID")
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/snippets", bad("route_path", "/old")), http.StatusBadRequest, "SNIPPET_INVALID")
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/snippets", bad("route_path", "/nope")), http.StatusBadRequest, "SNIPPET_INVALID")

	w = f.json(t, "admin", http.MethodPost, "/snippets", body)
	requireStatus(t, w, http.StatusOK, "")
	var row store.Snippet
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil || row.Pending != "ngx.say(\"<h1>Hello from Lua</h1>\")\n" || row.Content != "" {
		t.Fatalf("submitted: %v %s", err, w.Body)
	}
	// 未批准的脚本不参与渲染
	in, err := f.s.Deploy.Input(f.group, "r000009")
	if err != nil {
		t.Fatal(err)
	}
	if in.HasLua() {
		t.Fatal("pending script rendered")
	}

	approve := fmt.Sprintf("/snippets/%d/approve", row.ID)
	requireStatus(t, f.json(t, "operator", http.MethodPost, approve, nil), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, approve, nil), http.StatusForbidden, "SELF_APPROVAL")
	requireStatus(t, f.json(t, "admin2", http.MethodPost, approve, nil), http.StatusOK, "")

	in, err = f.s.Deploy.Input(f.group, "r000009")
	if err != nil {
		t.Fatal(err)
	}
	if !in.HasLua() {
		t.Fatal("approved script not rendered")
	}
	b, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for name, data := range b.Files {
		if strings.HasPrefix(name, "lua/script-test-") && strings.Contains(string(data), "Hello from Lua") {
			found = true
		}
	}
	if !found || !strings.Contains(string(b.Files["conf.d/site-test.conf"]), "content_by_lua_block") {
		t.Fatal("approved script missing from the bundle")
	}

	// 删除也要管理员提交、另一位管理员批准
	del := fmt.Sprintf("/snippets/%d/delete", row.ID)
	requireStatus(t, f.json(t, "operator", http.MethodPost, del, nil), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, del, nil), http.StatusOK, "")
	requireStatus(t, f.json(t, "admin2", http.MethodPost, approve, nil), http.StatusOK, "")
	if n, _ := q.Snippet.WithContext(ctx).Where(q.Snippet.ID.Eq(row.ID)).Count(); n != 0 {
		t.Fatal("script not deleted")
	}
	var audits int64
	f.s.Store.DB.Table("audit_logs").Where("action IN ? AND object_id = ?", []string{"snippet.submit", "snippet.approve", "snippet.submit_delete"}, fmt.Sprint(row.ID)).Count(&audits)
	if audits != 4 {
		t.Fatalf("audit entries = %d, want 4", audits)
	}
}
