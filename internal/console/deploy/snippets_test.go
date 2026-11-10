package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func addProxySite(t *testing.T, d *Service, g *store.NodeGroup) *store.Site {
	t.Helper()
	q := query.Use(d.Store.DB)
	ctx := context.Background()
	if _, err := q.NodeGroup.WithContext(ctx).Where(q.NodeGroup.ID.Eq(g.ID)).Update(q.NodeGroup.AllowedCIDRs, "10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	up, _ := json.Marshal(render.UpstreamGroup{Name: "api", Servers: []render.UpstreamServer{{Addr: "10.0.0.11:8080"}}})
	if err := q.Upstream.WithContext(ctx).Create(&store.Upstream{GroupID: g.ID, Name: "api", Spec: string(up)}); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(render.Site{Name: "shop", Domains: []string{"shop.example.com"},
		Routes: []render.Route{{Path: "/api/", Match: "prefix", Mode: "proxy", Proxy: &render.ProxySpec{Upstream: "api"}}}})
	s := &store.Site{GroupID: g.ID, Name: "shop", Spec: string(spec), Enabled: true}
	if err := q.Site.WithContext(ctx).Create(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func previewHas(t *testing.T, d *Service, g *store.NodeGroup, file, text string) bool {
	t.Helper()
	in, err := d.Input(g.ID, "r000000")
	if err != nil {
		t.Fatal(err)
	}
	b, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b.Files[file]), text)
}

func TestSnippetSubmitApprove(t *testing.T) {
	d, g := newTestService(t)
	site := addProxySite(t, d, g)

	// 禁用指令在提交时就被拦下，并指出行号
	_, issues, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, SiteID: site.ID, Scope: "server", Content: "gzip on;\ninclude /etc/passwd;"}, "alice", "")
	if err != nil || len(issues) != 1 || issues[0].Line != 2 {
		t.Fatalf("forbidden: %v %+v", err, issues)
	}
	sn, issues, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, SiteID: site.ID, Scope: "location", RoutePath: "/api/",
		Content: "proxy_read_timeout 300s;", Note: "慢接口"}, "alice", "")
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	// 待审批的片段不参与渲染
	if previewHas(t, d, g, "conf.d/site-shop.conf", "proxy_read_timeout") {
		t.Fatal("pending snippet must not be rendered")
	}
	if err := d.ApproveSnippet(sn.ID, "alice", ""); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approve: %v", err)
	}
	if err := d.ApproveSnippet(sn.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if !previewHas(t, d, g, "conf.d/site-shop.conf", "proxy_read_timeout  300s;") {
		t.Fatal("approved snippet should be rendered")
	}
	got, _ := d.GetSnippet(sn.ID)
	if got.Content == "" || got.ContentSHA256 == "" || got.Pending != "" || got.ApprovedBy != "bob" {
		t.Fatalf("after approve %+v", got)
	}

	// 修改：新内容待审批期间，线上仍是旧内容；驳回后保持旧内容
	if _, _, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, SiteID: site.ID, Scope: "location", RoutePath: "/api/", Content: "proxy_read_timeout 600s;"}, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if !previewHas(t, d, g, "conf.d/site-shop.conf", "300s") {
		t.Fatal("old content stays live while the change is pending")
	}
	if err := d.RejectSnippet(sn.ID, "bob", "太长", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetSnippet(sn.ID); got.Pending != "" || !strings.Contains(got.Content, "300s") {
		t.Fatalf("after reject %+v", got)
	}

	// 删除同样需要审批
	if err := d.SubmitSnippetDelete(sn.ID, "alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.ApproveSnippet(sn.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetSnippet(sn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted snippet still there: %v", err)
	}
	if previewHas(t, d, g, "conf.d/site-shop.conf", "proxy_read_timeout") {
		t.Fatal("deleted snippet still rendered")
	}
}

func TestSnippetHTTPAndValidationContext(t *testing.T) {
	d, g := newTestService(t)
	site := addProxySite(t, d, g)
	// 上游未登记、路由不存在都在提交时拒绝
	_, issues, _ := d.SubmitSnippet(SnippetInput{GroupID: g.ID, SiteID: site.ID, Scope: "server", Content: "location /x { proxy_pass http://up_nope; }"}, "alice", "")
	if len(issues) == 0 {
		t.Fatal("unknown upstream accepted")
	}
	if _, _, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, SiteID: site.ID, Scope: "location", RoutePath: "/missing/", Content: "expires 1h;"}, "alice", ""); err == nil {
		t.Fatal("missing route accepted")
	}
	if _, _, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, Scope: "http", Name: "Bad Name", Content: "gzip on;"}, "alice", ""); err == nil {
		t.Fatal("bad http snippet name accepted")
	}
	sn, issues, err := d.SubmitSnippet(SnippetInput{GroupID: g.ID, Scope: "http", Name: "gzip", Content: "gzip on; gzip_types text/css;"}, "alice", "")
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	if err := d.ApproveSnippet(sn.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if !previewHas(t, d, g, "conf.d/snippet-gzip.conf", "gzip_types  text/css;") {
		t.Fatal("http snippet not rendered")
	}
	stats, err := d.SnippetStats()
	if err != nil || stats["gzip"] != 1 || stats["gzip_types"] != 1 {
		t.Fatalf("stats %v %v", stats, err)
	}
	// 审计里有提交与批准，记录内容哈希
	rows, _ := query.Use(d.Store.DB).AuditLog.WithContext(context.Background()).Find()
	var acts []string
	for _, r := range rows {
		acts = append(acts, r.Action)
	}
	if j := strings.Join(acts, ","); !strings.Contains(j, "snippet.submit") || !strings.Contains(j, "snippet.approve") {
		t.Fatalf("audit %s", j)
	}
}

func TestSnippetValidationUsesGroupInstallRoot(t *testing.T) {
	d, g := newTestService(t)
	if err := d.SetProfile(g.ID, render.Profile{Mode: "system", Root: "/home/atwasoft/node"}, "alice", ""); err != nil {
		t.Fatal(err)
	}
	site := addProxySite(t, d, g)
	ctx, _, err := d.snippetContext(g.ID, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.SitesBase != "/home/atwasoft/node/conf/current/sites/" {
		t.Fatalf("sites base = %q", ctx.SitesBase)
	}
}
