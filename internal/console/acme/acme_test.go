package acme

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, Audit: &audit.Logger{DB: st.DB}}
}

func TestNormalize(t *testing.T) {
	got, err := normalize([]string{" Shop.Example.com ", "shop.example.com", "api.example.com", ""})
	if err != nil || strings.Join(got, ",") != "shop.example.com,api.example.com" {
		t.Fatalf("normalize: %v %v", got, err)
	}
	for _, bad := range [][]string{{}, {"*.example.com"}, {"bad domain"}, {"a..b"}} {
		if _, err := normalize(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// 域名必须有已发布的站点，否则 CA 的验证请求到不了节点。
func TestGroupsForRequiresPublishedSite(t *testing.T) {
	s := newSvc(t)
	s.Store.DB.Create(&store.NodeGroup{Name: "default"})
	spec, _ := json.Marshal(render.Site{Name: "shop", Domains: []string{"shop.example.com"}})
	s.Store.DB.Create(&store.Site{GroupID: 1, Name: "shop", Spec: string(spec), Enabled: true, Version: 1})
	if _, err := s.groupsFor([]string{"shop.example.com"}); err == nil || !strings.Contains(err.Error(), "已发布") {
		t.Fatalf("unpublished site accepted: %v", err)
	}
	b, err := render.Render(render.Input{Revision: "r000001", MimeTypes: []byte("types {}\n"),
		Sites: []render.Site{{Name: "shop", Domains: []string{"shop.example.com"}, Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	tgz, _ := b.TarGz()
	s.Store.DB.Create(&store.Revision{ID: "r000001", GroupID: 1, Bundle: tgz, Digest: b.Manifest.Digest})
	s.Store.DB.Model(&store.NodeGroup{}).Where("id = ?", 1).Update("current_revision_id", "r000001")
	gs, err := s.groupsFor([]string{"shop.example.com"})
	if err != nil || len(gs) != 1 || gs[0] != 1 {
		t.Fatalf("groups: %v %v", gs, err)
	}
	if _, err := s.groupsFor([]string{"shop.example.com", "blog.example.com"}); err == nil || !strings.Contains(err.Error(), "blog.example.com") {
		t.Fatalf("domain without a site accepted: %v", err)
	}
}

func TestDueBacksOffAfterFailure(t *testing.T) {
	s := newSvc(t)
	now := time.Now()
	soon := &store.Certificate{Name: "soon", Source: "acme", AutoRenew: true, NotAfter: now.Add(10 * 24 * time.Hour)}
	later := &store.Certificate{Name: "later", Source: "acme", AutoRenew: true, NotAfter: now.Add(60 * 24 * time.Hour)}
	manual := &store.Certificate{Name: "manual", Source: "upload", NotAfter: now.Add(5 * 24 * time.Hour)}
	off := &store.Certificate{Name: "off", Source: "acme", AutoRenew: false, NotAfter: now.Add(5 * 24 * time.Hour)}
	for _, c := range []*store.Certificate{soon, later, manual, off} {
		s.Store.DB.Create(c)
	}
	due := s.Due(now)
	if len(due) != 1 || due[0].Name != "soon" {
		t.Fatalf("due: %+v", due)
	}
	s.Store.DB.Create(&store.AcmeOrder{Name: "soon", CertID: soon.ID, Status: "failed"})
	if due := s.Due(now); len(due) != 0 {
		t.Fatal("retried within 6 hours of a failure")
	}
	if due := s.Due(now.Add(7 * time.Hour)); len(due) != 1 {
		t.Fatal("not retried after the back-off")
	}
}

func TestSettingsValidation(t *testing.T) {
	s := newSvc(t)
	if got := s.GetSettings().Directory; got != DefaultDirectory {
		t.Fatalf("default directory %s", got)
	}
	if err := s.SaveSettings("http://insecure.example/dir", "", "admin"); err == nil {
		t.Fatal("plain http directory accepted")
	}
	if err := s.SaveSettings("https://ca.internal/acme/directory", "ops@example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if st := s.GetSettings(); st.Directory != "https://ca.internal/acme/directory" || st.Email != "ops@example.com" || st.Account {
		t.Fatalf("settings: %+v", st)
	}
}
