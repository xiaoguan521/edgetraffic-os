package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func pair(t *testing.T, notAfter time.Time, hosts ...string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: hosts[0]},
		DNSNames: hosts, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}

func newSvc(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	st := storetest.Open(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, Box: box, Audit: &audit.Logger{DB: st.DB}}
}

func TestCreateValidatesAndEncrypts(t *testing.T) {
	s := newSvc(t)
	crt, key := pair(t, time.Now().Add(90*24*time.Hour), "shop.example.com", "*.shop.example.com")
	_, otherKey := pair(t, time.Now().Add(90*24*time.Hour), "x.example.com")
	oldCrt, oldKey := pair(t, time.Now().Add(-time.Hour), "old.example.com")

	if _, err := s.Create("bad", crt, otherKey, "admin"); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("mismatched key accepted: %v", err)
	}
	if _, err := s.Create("old", oldCrt, oldKey, "admin"); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expired cert accepted: %v", err)
	}
	if _, err := s.Create("../x", crt, key, "admin"); err == nil {
		t.Fatal("bad name accepted")
	}
	c, err := s.Create("shop", crt, key, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var raw store.Certificate
	s.Store.DB.First(&raw, c.ID)
	if strings.Contains(raw.KeyEnc, "PRIVATE KEY") || !strings.HasPrefix(raw.KeyEnc, "v1:") {
		t.Fatal("private key stored in plaintext")
	}
	if b, _ := json.Marshal(raw); strings.Contains(string(b), raw.KeyEnc) {
		t.Fatal("encrypted key leaks into JSON")
	}
	gotCrt, gotKey, err := s.Resolve(c.ID)
	if err != nil || gotCrt != crt || gotKey != key {
		t.Fatalf("resolve mismatch: %v", err)
	}
	if err := s.Covers(c.ID, []string{"shop.example.com", "a.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Covers(c.ID, []string{"shop.example.com", "blog.example.com"}); err == nil || !strings.Contains(err.Error(), "blog.example.com") {
		t.Fatalf("uncovered domain accepted: %v", err)
	}
	if _, err := s.Create("shop", crt, key, "admin"); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func site(t *testing.T, s *Service, name string, certID uint, domains ...string) {
	t.Helper()
	spec, _ := json.Marshal(render.Site{Name: name, Domains: domains, HTTPS: &render.TLS{CertID: certID}})
	if err := s.Store.DB.Create(&store.Site{GroupID: 1, Name: name, Spec: string(spec), Enabled: true, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRenewKeepsCoverageAndDeleteGuard(t *testing.T) {
	s := newSvc(t)
	s.Store.DB.Create(&store.NodeGroup{Name: "default"})
	crt, key := pair(t, time.Now().Add(10*24*time.Hour), "shop.example.com")
	c, err := s.Create("shop", crt, key, "admin")
	if err != nil {
		t.Fatal(err)
	}
	site(t, s, "shop", c.ID, "shop.example.com")

	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Sites != 1 || list[0].Status != "expiring" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if gs := s.Groups(c.ID); len(gs) != 1 || gs[0] != 1 {
		t.Fatalf("groups: %v", gs)
	}

	// 续期证书不再覆盖站点域名 → 拒绝
	wrongCrt, wrongKey := pair(t, time.Now().Add(90*24*time.Hour), "other.example.com")
	if _, err := s.Renew(c.ID, wrongCrt, wrongKey, "admin"); err == nil || !strings.Contains(err.Error(), "shop.example.com") {
		t.Fatalf("renew dropping a used domain accepted: %v", err)
	}
	newCrt, newKey := pair(t, time.Now().Add(90*24*time.Hour), "shop.example.com")
	r, err := s.Renew(c.ID, newCrt, newKey, "admin")
	if err != nil || r.NotAfter.Before(time.Now().Add(80*24*time.Hour)) {
		t.Fatalf("renew: %+v %v", r, err)
	}
	if got, _, _ := s.Resolve(c.ID); got != newCrt {
		t.Fatal("renew did not replace the certificate")
	}
	if err := s.Delete(c.ID, "admin"); err == nil || !strings.Contains(err.Error(), "shop") {
		t.Fatalf("delete of a certificate in use accepted: %v", err)
	}
}

func TestMigrateInline(t *testing.T) {
	s := newSvc(t)
	crt, key := pair(t, time.Now().Add(90*24*time.Hour), "blog.example.com")
	spec, _ := json.Marshal(render.Site{Name: "blog", Domains: []string{"blog.example.com"}, HTTPS: &render.TLS{CertPEM: crt, KeyPEM: key}})
	s.Store.DB.Create(&store.Site{GroupID: 1, Name: "blog", Spec: string(spec), Enabled: true, Version: 1})

	if err := s.MigrateInline(); err != nil {
		t.Fatal(err)
	}
	var st store.Site
	s.Store.DB.Where("name = ?", "blog").First(&st)
	var rs render.Site
	_ = json.Unmarshal([]byte(st.Spec), &rs)
	if rs.HTTPS == nil || rs.HTTPS.CertID == 0 || rs.HTTPS.CertPEM != "" || rs.HTTPS.KeyPEM != "" {
		t.Fatalf("site not migrated: %+v", rs.HTTPS)
	}
	if strings.Contains(st.Spec, "PRIVATE KEY") {
		t.Fatal("plaintext key left in site spec")
	}
	if got, gotKey, err := s.Resolve(rs.HTTPS.CertID); err != nil || got != crt || gotKey != key {
		t.Fatal("migrated certificate does not resolve to the original PEM")
	}
	if err := s.MigrateInline(); err != nil { // 幂等
		t.Fatal(err)
	}
	var n int64
	s.Store.DB.Model(&store.Certificate{}).Count(&n)
	if n != 1 {
		t.Fatalf("migration not idempotent: %d certificates", n)
	}
}
