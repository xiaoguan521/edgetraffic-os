package certs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/sm2cert/sm2certtest"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func gmInput(t *testing.T, hosts ...string) GMInput {
	t.Helper()
	s := sm2certtest.New(t, hosts[0], "sign", hosts)
	e := sm2certtest.New(t, hosts[0], "enc", hosts)
	return GMInput{SignCertPEM: s.CertPEM, SignKeyPEM: s.KeyPEM, EncCertPEM: e.CertPEM, EncKeyPEM: e.KeyPEM}
}

func TestGMCertLifecycle(t *testing.T) {
	s := newSvc(t)
	in := gmInput(t, "gm.example.com")

	swapped := GMInput{SignCertPEM: in.EncCertPEM, SignKeyPEM: in.EncKeyPEM, EncCertPEM: in.SignCertPEM, EncKeyPEM: in.SignKeyPEM}
	if _, err := s.CreateGM("gm", swapped, "admin"); err == nil || !strings.Contains(err.Error(), "放反") {
		t.Fatalf("swapped pair accepted: %v", err)
	}
	c, err := s.CreateGM("gm", in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != store.KindSM2 || c.EncFingerprint == "" || strings.Contains(c.KeyEnc, "PRIVATE") || strings.Contains(c.EncKeyEnc, "PRIVATE") {
		t.Fatalf("stored %+v", c)
	}
	std, key := pair(t, time.Now().Add(90*24*time.Hour), "gm.example.com")
	stdC, err := s.Create("std", std, key, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// 站点：标准证书 + 国密双证书；类型错用要拒绝
	rs := render.Site{Name: "gm", Domains: []string{"gm.example.com"}, HTTPS: &render.TLS{CertID: stdC.ID, GMCertID: c.ID}}
	if err := s.ResolveSite(&rs); err != nil {
		t.Fatal(err)
	}
	if rs.HTTPS.GM == nil || rs.HTTPS.GM.SignKeyPEM != strings.TrimSpace(in.SignKeyPEM)+"\n" || !strings.Contains(rs.HTTPS.GM.EncCertPEM, "BEGIN CERTIFICATE") {
		t.Fatal("GM pair not resolved")
	}
	for _, bad := range []render.TLS{{CertID: c.ID}, {CertID: stdC.ID, GMCertID: stdC.ID}} {
		b := bad
		r := render.Site{Name: "x", Domains: []string{"gm.example.com"}, HTTPS: &b}
		if err := s.ResolveSite(&r); err == nil {
			t.Errorf("wrong kind accepted: %+v", bad)
		}
	}
	other := render.Site{Name: "x", Domains: []string{"other.example.com"}, HTTPS: &render.TLS{CertID: stdC.ID, GMCertID: c.ID}}
	if err := s.ResolveSite(&other); err == nil {
		t.Error("GM cert not covering the domain accepted")
	}

	// 被站点引用后不能删除；续期必须仍覆盖站点域名；国密证书不能用标准续期接口
	spec, _ := json.Marshal(render.Site{Name: "gm", Domains: []string{"gm.example.com"}, HTTPS: &render.TLS{CertID: stdC.ID, GMCertID: c.ID}})
	if err := s.Store.DB.Create(&store.Site{GroupID: 1, Name: "gm", Spec: string(spec), Enabled: true, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(c.ID, "admin"); err == nil {
		t.Fatal("in-use GM cert deleted")
	}
	if g := s.Groups(c.ID); len(g) != 1 || g[0] != 1 {
		t.Fatalf("groups = %v", g)
	}
	if _, err := s.RenewGM(c.ID, gmInput(t, "elsewhere.example.com"), "admin"); err == nil {
		t.Fatal("renewal dropping a site domain accepted")
	}
	if _, err := s.Renew(c.ID, std, key, "admin"); err == nil {
		t.Fatal("standard renewal of a GM cert accepted")
	}
	if _, err := s.RenewGM(stdC.ID, in, "admin"); err == nil {
		t.Fatal("GM renewal of a standard cert accepted")
	}
	nc, err := s.RenewGM(c.ID, gmInput(t, "gm.example.com"), "admin")
	if err != nil || nc.Fingerprint == c.Fingerprint {
		t.Fatalf("renew: %v", err)
	}
	if v, _ := s.List(); len(v) != 2 {
		t.Fatalf("list = %d", len(v))
	}
}
