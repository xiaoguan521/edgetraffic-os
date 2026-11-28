package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/conn"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		b := []byte(body)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAgentPushConfigSnapshotEndToEndMTLS(t *testing.T) {
	st := storetest.Open(t)
	st.DB.Create(&store.NodeGroup{Name: "standalone-grp", AllowedCIDRs: "10.0.0.0/8"})
	aud := &audit.Logger{DB: st.DB}
	licPub, licPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lic := &licsvc.Service{Store: st, DataDir: dir, Keys: license.KeyRing{"etos-ci-agent-import": licPub}}
	if err := lic.Init(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rawLic, err := license.Sign(&license.Payload{
		V: 1, KID: "etos-ci-agent-import", LicenseID: "agent-import-test",
		Customer: license.Customer{Name: "test"}, Edition: license.EditionProfessional,
		Features: []license.Feature{license.FeatureCore}, Limits: license.Limits{Nodes: 10, Sites: 50},
		IssuedAt: now.Add(-48 * time.Hour), NotBefore: now.Add(-48 * time.Hour), ExpiresAt: now.Add(365 * 24 * time.Hour),
		Binding: license.Binding{Type: "installation_only", InstallationID: lic.InstallationID()},
	}, licPriv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lic.Import(rawLic, "admin"); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreate(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	_, cmdPriv, _ := ed25519.GenerateKey(rand.Reader)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	box, err := secretbox.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	crt := &certs.Service{Store: st, Box: box, Audit: aud}
	gw := &gateway.Server{Store: st, CA: ca, CmdKey: cmdPriv, Lic: lic, Audit: aud, Log: log, Box: box, SnapshotMinInterval: -1}
	dep := &deploy.Service{Store: st, Certs: crt, GW: gw, Audit: aud, Log: log, Box: box, MimeTypes: render.DefaultMimeTypes()}
	if err := dep.SetManagementMode(1, deploy.ManagementModeObserve, "admin", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	apiSrv := &Server{Store: st, GW: gw, Deploy: dep, Certs: crt, Lic: lic, Audit: aud, Log: log}
	gw.OnConfigSnapshot = apiSrv.HandleAgentConfigSnapshot

	tlsCfg, err := gw.TLSConfig([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	gs := gw.GRPCServer(tlsCfg)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		gs.Stop()
		_ = lis.Close()
	}()
	go func() { _ = gs.Serve(lis) }()

	tok, _, err := gw.NewEnrollToken(1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ag := conn.New(conn.Config{Server: lis.Addr().String(), StateDir: t.TempDir()}, log)
	if err := ag.Enroll(context.Background(), tok, pki.Fingerprint(ca.Cert.Raw)); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	// 1. 首次上报干净独立配置：空组 + observe 模式应自动生成草稿，且不触发任何发布
	cleanArchive := makeTarGz(t, map[string]string{
		brand.ConfName(): "http {\n  include conf.d/*.conf;\n}\n",
		"conf.d/app.conf": `
upstream api_backend {
  server 10.1.2.3:8080;
}
server {
  listen 80;
  server_name shop.example.com;
  location / {
    proxy_pass http://api_backend;
  }
}
`,
	})
	resp, err := ag.PushConfigSnapshot(context.Background(), cleanArchive)
	if err != nil {
		t.Fatalf("PushConfigSnapshot failed: %v", err)
	}
	if !resp.AutoApplied || resp.AppliedSites != 1 || resp.AppliedUpstreams != 1 || resp.ImportId == 0 {
		var impRow store.ConfigImport
		st.DB.First(&impRow, resp.ImportId)
		t.Fatalf("expected auto-applied 1 site + 1 upstream, got %+v\nresult=%s\nreport=%s", resp, impRow.Result, impRow.Report)
	}
	var sites []store.Site
	st.DB.Where("group_id = ?", 1).Find(&sites)
	if len(sites) != 1 || !sites[0].Enabled {
		t.Fatalf("expected 1 enabled draft site, got %+v", sites)
	}
	var depCount int64
	st.DB.Model(&store.Deployment{}).Count(&depCount)
	if depCount != 0 {
		t.Fatalf("auto import must never trigger a deployment, got %d", depCount)
	}

	// 2. 组内已有草稿后再次上报：仅生成报告，不自动覆盖已有草稿
	resp2, err := ag.PushConfigSnapshot(context.Background(), cleanArchive)
	if err != nil {
		t.Fatalf("second PushConfigSnapshot failed: %v", err)
	}
	if resp2.AutoApplied {
		t.Fatalf("expected AutoApplied=false when group already has draft sites, got %+v", resp2)
	}

	// 3. 含原生 Lua 的配置：标记 RequiresLegacy 且不自动落托管草稿
	st.DB.Where("group_id = ?", 1).Delete(&store.Site{})
	st.DB.Where("group_id = ?", 1).Delete(&store.Upstream{})
	luaArchive := makeTarGz(t, map[string]string{
		brand.ConfName(): "http {\n  include conf.d/*.conf;\n}\n",
		"lua/app.lua":    "ngx.say('hi')\n",
		"conf.d/lua.conf": `
server {
  listen 80;
  server_name lua.example.com;
  location / {
    content_by_lua_file lua/app.lua;
  }
}
`,
	})
	respLua, err := ag.PushConfigSnapshot(context.Background(), luaArchive)
	if err != nil {
		t.Fatalf("lua PushConfigSnapshot failed: %v", err)
	}
	if !respLua.RequiresLegacy || respLua.AutoApplied {
		t.Fatalf("expected RequiresLegacy=true and AutoApplied=false, got %+v", respLua)
	}

	// 4. 含 HTTPS 证书与私钥上报：证书加密写入证书库，报告 JSON 中绝不含私钥明文，内存暂存立即清空
	st.DB.Where("group_id = ?", 1).Delete(&store.Site{})
	st.DB.Where("group_id = ?", 1).Delete(&store.Upstream{})
	eck, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tls.example.com"},
		DNSNames:     []string{"tls.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &eck.PublicKey, eck)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(eck)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	tlsArchive := makeTarGz(t, map[string]string{
		brand.ConfName():  "http {\n  include conf.d/*.conf;\n}\n",
		"certs/tls.crt":   certPEM,
		"certs/tls.key":   keyPEM,
		"conf.d/tls.conf": "upstream tlsu { server 10.1.2.4:8080; }\nserver {\n  listen 443 ssl;\n  server_name tls.example.com;\n  ssl_certificate certs/tls.crt;\n  ssl_certificate_key certs/tls.key;\n  location / { proxy_pass http://tlsu; }\n}\n",
	})
	respTLS, err := ag.PushConfigSnapshot(context.Background(), tlsArchive)
	if err != nil {
		t.Fatalf("tls PushConfigSnapshot failed: %v", err)
	}
	if !respTLS.AutoApplied || respTLS.AppliedCerts != 1 || respTLS.AppliedSites != 1 {
		t.Fatalf("expected auto-applied cert+site, got %+v", respTLS)
	}
	var impTLS store.ConfigImport
	st.DB.First(&impTLS, respTLS.ImportId)
	if strings.Contains(impTLS.Report, "PRIVATE KEY") {
		t.Fatalf("import report must never contain private key plaintext")
	}
	if _, stillPending := importCerts.Load(uint(respTLS.ImportId)); stillPending {
		t.Fatalf("pending cert material should be cleared immediately after auto-apply")
	}
	var certCnt int64
	st.DB.Model(&store.Certificate{}).Count(&certCnt)
	if certCnt != 1 {
		t.Fatalf("expected 1 encrypted certificate stored, got %d", certCnt)
	}

	// 5. 两台独立节点并发上报空组：互斥锁保证只有 1 台自动落草稿，不产生重复站点
	st.DB.Where("group_id = ?", 1).Delete(&store.Site{})
	st.DB.Where("group_id = ?", 1).Delete(&store.Upstream{})
	tok2, _, err := gw.NewEnrollToken(1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ag2 := conn.New(conn.Config{Server: lis.Addr().String(), StateDir: t.TempDir()}, log)
	if err := ag2.Enroll(context.Background(), tok2, pki.Fingerprint(ca.Cert.Raw)); err != nil {
		t.Fatalf("enroll node 2 failed: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	autoCount := 0
	for _, a := range []*conn.Agent{ag, ag2} {
		wg.Add(1)
		go func(agent *conn.Agent) {
			defer wg.Done()
			r, perr := agent.PushConfigSnapshot(context.Background(), cleanArchive)
			if perr != nil {
				t.Errorf("concurrent push failed: %v", perr)
				return
			}
			if r.AutoApplied {
				mu.Lock()
				autoCount++
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	if autoCount != 1 {
		t.Fatalf("expected exactly 1 concurrent auto-apply, got %d", autoCount)
	}
	var finalSites int64
	st.DB.Model(&store.Site{}).Where("group_id = ?", 1).Count(&finalSites)
	if finalSites != 1 {
		t.Fatalf("expected exactly 1 site after concurrent push, got %d", finalSites)
	}

	// 6. 单节点限频保护：开启 SnapshotMinInterval 后立即连推应返回 ResourceExhausted
	gw.SnapshotMinInterval = 30 * time.Second
	if _, err := ag.PushConfigSnapshot(context.Background(), cleanArchive); err != nil {
		t.Fatalf("first rate-limited push should succeed: %v", err)
	}
	if _, err := ag.PushConfigSnapshot(context.Background(), cleanArchive); err == nil {
		t.Fatalf("immediate second push should be rate-limited")
	}
}
