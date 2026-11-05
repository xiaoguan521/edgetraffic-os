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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// These tests use the real HTTP routing/auth/license middleware and database.
// They run against SQLite or an isolated PostgreSQL schema via ETOS_TEST_DB.
type importHTTP struct {
	s       *Server
	h       http.Handler
	cookies map[string]*http.Cookie
	key     ed25519.PrivateKey
	group   uint
}

func newImportHTTP(t *testing.T) *importHTTP {
	t.Helper()
	st := storetest.Open(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lic := &licsvc.Service{Store: st, DataDir: dir, Keys: license.KeyRing{"etos-ci-import": pub}}
	if err := lic.Init(); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	a := &audit.Logger{DB: st.DB}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cs := &certs.Service{Store: st, Box: box, Audit: a}
	gw := &gateway.Server{Store: st, Lic: lic, Audit: a, Log: log}
	d := &deploy.Service{Store: st, Certs: cs, GW: gw, Audit: a, Log: log, Box: box, MimeTypes: render.DefaultMimeTypes()}
	s := &Server{Store: st, Lic: lic, Certs: cs, Deploy: d, GW: gw, Audit: a, Log: log}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	f := &importHTTP{s: s, h: h, key: priv, cookies: map[string]*http.Cookie{}}
	f.setLicense(t, 10, time.Now().Add(365*24*time.Hour))
	g := &store.NodeGroup{Name: "imports", AllowedCIDRs: "10.0.0.0/8"}
	q := query.Use(st.DB)
	if err := q.NodeGroup.WithContext(context.Background()).Create(g); err != nil {
		t.Fatal(err)
	}
	f.group = g.ID
	hash, err := crypto.HashPassword("Import@2026!test")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{store.RoleAdmin, store.RoleOperator, store.RoleReleaser, store.RoleViewer} {
		now := time.Now()
		u := &store.User{Username: role, Role: role, Source: "local", PasswordHash: hash, PasswordChangedAt: &now}
		if err := q.User.WithContext(context.Background()).Create(u); err != nil {
			t.Fatal(err)
		}
		w := f.json(t, "", http.MethodPost, "/auth/login", map[string]string{"username": role, "password": "Import@2026!test"})
		requireStatus(t, w, 200, "")
		for _, c := range w.Result().Cookies() {
			if c.Name == cookieName {
				f.cookies[role] = c
			}
		}
		if f.cookies[role] == nil {
			t.Fatal("login did not set session cookie")
		}
	}
	return f
}

func (f *importHTTP) setLicense(t *testing.T, sites int, expires time.Time) {
	t.Helper()
	now := time.Now().UTC()
	raw, err := license.Sign(&license.Payload{V: 1, KID: "etos-ci-import", LicenseID: "import-test", Customer: license.Customer{Name: "test"}, Edition: license.EditionProfessional, Features: []license.Feature{license.FeatureCore}, Limits: license.Limits{Sites: sites}, IssuedAt: now.Add(-48 * time.Hour), NotBefore: now.Add(-48 * time.Hour), ExpiresAt: expires, Binding: license.Binding{Type: "installation_only", InstallationID: f.s.Lic.InstallationID()}}, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Lic.Import(raw, "admin"); err != nil {
		t.Fatal(err)
	}
}

func (f *importHTTP) request(role, method, path string, b []byte, ct string, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(b))
	if c := f.cookies[role]; c != nil {
		r.AddCookie(c)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	if csrf {
		r.Header.Set("X-Requested-With", "etos")
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func (f *importHTTP) json(t *testing.T, role, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(role, method, path, b, "application/json", true)
}
func uploadBody(t *testing.T, gid uint, name string, data []byte) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if err := mw.WriteField("group_id", fmt.Sprint(gid)); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		p, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), mw.FormDataContentType()
}
func (f *importHTTP) upload(t *testing.T, role, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	b, ct := uploadBody(t, f.group, name, data)
	return f.request(role, "POST", "/imports", b, ct, true)
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body)
	}
	if code != "" {
		var e struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != code {
			t.Fatalf("want error %s: %s", code, w.Body)
		}
	}
}
func decodeImport(t *testing.T, w *httptest.ResponseRecorder) store.ConfigImport {
	t.Helper()
	requireStatus(t, w, 200, "")
	var d struct {
		Import store.ConfigImport `json:"import"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Import.ID == 0 {
		t.Fatal("missing import id")
	}
	return d.Import
}

const importDump = `# configuration file /etc/web/web.conf:
http {
 upstream legacy { server 10.0.0.11:8080; }
 server {
  listen 80;
  server_name imported.example.com;
  client_max_body_size 8m;
  location / { proxy_pass http://legacy; }
 }
}
`

// IMP-01/04/05/06: HTTP upload -> report -> draft -> two-person snippet approval -> rendered preview.
func TestImportHTTPDraftWorkflow(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	q := query.Use(f.s.Store.DB)
	ctx := context.Background()
	if n, err := q.Site.WithContext(ctx).Count(); err != nil || n != 0 {
		t.Fatal("upload changed site drafts", n, err)
	}
	b := f.json(t, "operator", "GET", fmt.Sprintf("/imports/%d/report", row.ID), nil)
	requireStatus(t, b, 200, "")
	if b.Header().Get("Content-Disposition") == "" || !strings.Contains(b.Body.String(), "client_max_body_size") {
		t.Fatal("download missing report or filename")
	}
	var result ApplyResult
	w := f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Sites) != 1 || !result.Sites[0].Enabled || len(result.Snippets) != 1 {
		t.Fatalf("apply result %+v", result)
	}
	g, err := store.First[store.NodeGroup](f.s.Store, f.group)
	if err != nil || g.CurrentRevisionID != "" {
		t.Fatal("applying draft changed live revision", err)
	}
	pv := f.json(t, "operator", "GET", fmt.Sprintf("/node-groups/%d/preview", f.group), nil)
	requireStatus(t, pv, 200, "")
	if strings.Contains(pv.Body.String(), "client_max_body_size  8m;") {
		t.Fatal("unapproved snippet rendered")
	}
	snid := result.Snippets[0]
	requireStatus(t, f.json(t, "operator", "POST", fmt.Sprintf("/snippets/%d/approve", snid), map[string]any{}), 403, "SELF_APPROVAL")
	requireStatus(t, f.json(t, "admin", "POST", fmt.Sprintf("/snippets/%d/approve", snid), map[string]any{}), 200, "")
	pv = f.json(t, "operator", "GET", fmt.Sprintf("/node-groups/%d/preview", f.group), nil)
	requireStatus(t, pv, 200, "")
	if !strings.Contains(pv.Body.String(), "client_max_body_size  8m;") {
		t.Fatal("approved snippet missing")
	}
	requireStatus(t, f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), 409, "ALREADY_APPLIED")
	if n, err := q.Site.WithContext(ctx).Count(); err != nil || n != 1 {
		t.Fatal("duplicate application changed site count", n, err)
	}
}

// IMP-08, AUTH-01/08: test the actual router, not a test-only role middleware.
func TestImportHTTPAccessMatrix(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	for _, role := range []string{"", "viewer", "releaser", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			want := 200
			switch role {
			case "":
				want = 401
			case "viewer", "releaser":
				want = 403
			}
			for _, path := range []string{"/imports", fmt.Sprintf("/imports/%d", row.ID), fmt.Sprintf("/imports/%d/report", row.ID)} {
				requireStatus(t, f.json(t, role, "GET", path, nil), want, "")
			}
			requireStatus(t, f.upload(t, role, "access.txt", []byte(importDump)), want, "")
			if want != 200 {
				requireStatus(t, f.json(t, role, "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), want, "")
			}
		})
	}
	b, ct := uploadBody(t, f.group, "csrf.txt", []byte(importDump))
	requireStatus(t, f.request("operator", "POST", "/imports", b, ct, false), 403, "CSRF")
}

// IMP-08, LIC-07: read reports while expired; block both upload and applying a pre-existing report.
func TestImportHTTPReadOnly(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	f.setLicense(t, 10, time.Now().Add(-24*time.Hour))
	if f.s.Lic.Snapshot().Writable() {
		t.Fatal("fixture license should be read-only")
	}
	requireStatus(t, f.upload(t, "operator", "blocked.txt", []byte(importDump)), 423, "LICENSE_READ_ONLY")
	requireStatus(t, f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), 423, "LICENSE_READ_ONLY")
	requireStatus(t, f.json(t, "operator", "GET", fmt.Sprintf("/imports/%d/report", row.ID), nil), 200, "")
	f.setLicense(t, 10, time.Now().Add(365*24*time.Hour))
	requireStatus(t, f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), 200, "")
}

// IMP-07: invalid HTTP uploads must not create history rows or drafts.
func TestImportHTTPInvalidUploads(t *testing.T) {
	f := newImportHTTP(t)
	for _, tc := range []struct {
		name, file string
		data       []byte
		status     int
		code       string
	}{
		{"missing-file", "", nil, 400, "NO_FILE"},
		{"invalid-text", "bad.conf", []byte("http {"), 400, "IMPORT_PARSE_FAILED"},
		{"broken-gzip", "bad.tgz", []byte{0x1f, 0x8b, 0, 0}, 400, "IMPORT_PARSE_FAILED"},
		{"oversize", "large.txt", bytes.Repeat([]byte("x"), maxImportUpload+1), 413, "TOO_LARGE"},
		{"traversal", "unsafe.tgz", importArchive(t, map[string]string{"../escape.conf": "http {}"}), 400, "IMPORT_PARSE_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireStatus(t, f.upload(t, "operator", tc.file, tc.data), tc.status, tc.code) })
	}
	q := query.Use(f.s.Store.DB)
	ctx := context.Background()
	if n, err := q.ConfigImport.WithContext(ctx).Count(); err != nil || n != 0 {
		t.Fatal("invalid uploads wrote history", n, err)
	}
	if n, err := q.Site.WithContext(ctx).Count(); err != nil || n != 0 {
		t.Fatal("invalid uploads wrote drafts", n, err)
	}
}
func importArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// IMP-05: concurrent double-clicks may claim an import only once.
func TestImportHTTPConcurrentApply(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.request("operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), []byte("{}"), "application/json", true).Code
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("duplicate apply statuses %+v", counts)
	}
	q := query.Use(f.s.Store.DB)
	if n, err := q.Site.WithContext(context.Background()).Count(); err != nil || n != 1 {
		t.Fatal("duplicate drafts", n, err)
	}
}

// IMP-02, CERT-04: certificates stay out of reports and enter encrypted storage only on confirmation.
func TestImportHTTPArchiveSecrets(t *testing.T) {
	f := newImportHTTP(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "secure.example.com"}, DNSNames: []string{"secure.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	crt := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	secret := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	data := importArchive(t, map[string]string{"web/web.conf": `events {} http { server { listen 443 ssl; server_name secure.example.com; ssl_certificate /etc/web/certs/site.crt; ssl_certificate_key /etc/web/certs/site.key; location / { deny all; } } }`, "web/certs/site.crt": crt, "web/certs/site.key": secret})
	w := f.upload(t, "operator", "secure.tgz", data)
	row := decodeImport(t, w)
	assertPrivate := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if strings.Contains(w.Body.String(), "PRIVATE KEY") || strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
			t.Fatal("certificate material leaked in HTTP response")
		}
	}
	assertPrivate(w)
	for _, path := range []string{"/imports", fmt.Sprintf("/imports/%d", row.ID), fmt.Sprintf("/imports/%d/report", row.ID)} {
		w := f.json(t, "operator", "GET", path, nil)
		requireStatus(t, w, 200, "")
		assertPrivate(w)
	}
	w = f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	assertPrivate(w)
	var result ApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Certs) != 1 || len(result.Errors) != 0 {
		t.Fatalf("certificate not imported: %+v", result)
	}
	q := query.Use(f.s.Store.DB)
	cs, err := q.Certificate.WithContext(context.Background()).Find()
	if err != nil || len(cs) != 1 {
		t.Fatal("missing certificate", err)
	}
	if strings.Contains(cs[0].KeyEnc, "PRIVATE KEY") || !strings.HasPrefix(cs[0].KeyEnc, "v1:") {
		t.Fatal("private key not encrypted")
	}
	_, resolved, err := f.s.Certs.Resolve(cs[0].ID)
	if err != nil || resolved != secret {
		t.Fatal("stored certificate cannot be decrypted", err)
	}
}

// IMP-03/08, LIC-03: filtering is by group and quota checks also apply to bulk import.
func TestImportHTTPGroupAndQuota(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "first.conf", []byte(importDump)))
	w := f.json(t, "operator", "GET", fmt.Sprintf("/imports?group_id=%d", f.group), nil)
	requireStatus(t, w, 200, "")
	var rows []store.ConfigImport
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].ID != row.ID {
		t.Fatal("missing group history", err)
	}
	w = f.json(t, "operator", "GET", "/imports?group_id=999999", nil)
	requireStatus(t, w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 0 {
		t.Fatal("cross-group history", err)
	}
	f.setLicense(t, 1, time.Now().Add(365*24*time.Hour))
	requireStatus(t, f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), 200, "")
	next := decodeImport(t, f.upload(t, "operator", "second.conf", []byte(importDump)))
	w = f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", next.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	var result ApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sites) != 0 || len(result.Errors) != 1 {
		t.Fatalf("import bypassed quota: %+v", result)
	}
	q := query.Use(f.s.Store.DB)
	if n, err := q.Site.WithContext(context.Background()).Count(); err != nil || n != 1 {
		t.Fatal("quota exceeded", n, err)
	}
}

// IMP-04/06: unsupported directives must carry file/line and never become enabled sites.
func TestImportHTTPBlockingReport(t *testing.T) {
	f := newImportHTTP(t)
	dump := strings.Replace(importDump, "location / { proxy_pass http://legacy; }", "location / { proxy_pass http://legacy; }\n  location ~ \\.php$ { fastcgi_pass 127.0.0.1:9000; }", 1)
	row := decodeImport(t, f.upload(t, "operator", "partial.conf", []byte(dump)))
	if row.Partial != 1 || row.Issues == 0 {
		t.Fatalf("missing partial summary: %+v", row)
	}
	w := f.json(t, "operator", "POST", fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	var result ApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sites) != 1 || result.Sites[0].Enabled || len(result.Errors) != 0 {
		t.Fatalf("unsafe site should stay disabled: %+v", result)
	}
}
