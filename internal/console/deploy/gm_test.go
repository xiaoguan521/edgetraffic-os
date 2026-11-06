package deploy

import (
	"context"
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

	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/sm2cert/sm2certtest"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func stdPair(t *testing.T, host string) (string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}

func gmIn(t *testing.T, host string) certs.GMInput {
	s := sm2certtest.New(t, host, "sign", []string{host})
	e := sm2certtest.New(t, host, "enc", []string{host})
	return certs.GMInput{SignCertPEM: s.CertPEM, SignKeyPEM: s.KeyPEM, EncCertPEM: e.CertPEM, EncKeyPEM: e.KeyPEM}
}

func TestGMSitePublishAndRenew(t *testing.T) {
	d, g := newTestService(t)
	box, _ := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	d.Certs = &certs.Service{Store: d.Store, Box: box, Audit: d.Audit}
	licensed := false
	d.Feature = func(f string) bool { return f != "gm_tls" || licensed }
	q := query.Use(d.Store.DB)
	ctx := context.Background()

	c, k := stdPair(t, "gm.example.com")
	std, err := d.Certs.Create("std", c, k, "alice")
	if err != nil {
		t.Fatal(err)
	}
	gm, err := d.Certs.CreateGM("gm", gmIn(t, "gm.example.com"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(render.Site{Name: "gm", Domains: []string{"gm.example.com"}, HTTPS: &render.TLS{CertID: std.ID, GMCertID: gm.ID},
		Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}})
	if err := q.Site.WithContext(ctx).Create(&store.Site{GroupID: g.ID, Name: "gm", Spec: string(spec), Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := d.Preview(g.ID); err == nil || !strings.Contains(err.Error(), "LICENSE_FEATURE_DENIED") {
		t.Fatalf("unlicensed: %v", err)
	}
	licensed = true
	// 组内还没有节点时同样先报国密约束（控制台冒烟用例依赖这个顺序）
	if _, err := d.StartUpgrade(g.ID, "1.30.6-1", "standard", "alice", ""); err == nil || !strings.Contains(err.Error(), "国密") {
		t.Fatalf("standard upgrade of a GM group without nodes: %v", err)
	}
	node := &store.Node{GroupID: g.ID, Name: "edge-1", Status: "offline", TLSLib: "openssl"}
	if err := q.Node.WithContext(ctx).Create(node); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Preview(g.ID); err == nil || !strings.Contains(err.Error(), "GM_VARIANT_REQUIRED") || !strings.Contains(err.Error(), "edge-1") {
		t.Fatalf("standard node: %v", err)
	}
	if _, err := d.StartUpgrade(g.ID, "1.30.6-1", "standard", "alice", ""); err == nil || !strings.Contains(err.Error(), "国密") {
		t.Fatalf("standard upgrade of a GM group: %v", err)
	}
	if _, err := q.Node.WithContext(ctx).Where(q.Node.ID.Eq(node.ID)).Update(q.Node.TLSLib, "tongsuo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Preview(g.ID); err != nil {
		t.Fatalf("GM node: %v", err)
	}

	// 发布（节点吊销后组内无节点，只生成版本）
	_, _ = q.Node.WithContext(ctx).Where(q.Node.ID.Eq(node.ID)).Update(q.Node.Status, "revoked")
	dep, err := d.Publish(g.ID, "gm", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep)
	r1, _ := d.revision(currentRev(t, d, g), g.ID)
	f1, _ := render.ReadTarGz(r1.Bundle)
	if !strings.Contains(string(f1["conf.d/site-gm.conf"]), render.ReleasesDir+r1.ID+"/certs/site-gm.sm2-sign.crt") {
		t.Fatal("GM directives missing")
	}

	// 国密证书续期：只替换四个国密文件并改写版本路径，agent 视为只换证书
	if _, err := d.Certs.RenewGM(gm.ID, gmIn(t, "gm.example.com"), "alice"); err != nil {
		t.Fatal(err)
	}
	dep, err = d.RenewalRefresh(g.ID, gm.ID, "alice", "")
	if err != nil || dep == nil {
		t.Fatalf("refresh: %v %v", dep, err)
	}
	waitDone(t, d, dep)
	r2, _ := d.revision(currentRev(t, d, g), g.ID)
	f2, _ := render.ReadTarGz(r2.Bundle)
	if r2.ID == r1.ID || string(f2["certs/site-gm.sm2-sign.crt"]) == string(f1["certs/site-gm.sm2-sign.crt"]) {
		t.Fatal("renewal produced no new GM cert")
	}
	if !strings.Contains(string(f2["conf.d/site-gm.conf"]), render.ReleasesDir+r2.ID+"/certs/") {
		t.Fatal("release path not rebased")
	}
	if !render.CertOnlyChange(f1, f2) {
		t.Fatal("agent would reject the GM renewal")
	}
}
