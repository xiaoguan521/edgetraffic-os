package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
)

func TestLegacyPackageNeedsTwoAdministratorsHTTP(t *testing.T) {
	f := newImportHTTP(t)
	now := time.Now().UTC()
	raw, err := license.Sign(&license.Payload{V: 1, KID: "etos-ci-import", LicenseID: "legacy-test", Customer: license.Customer{Name: "test"},
		Edition: license.EditionProfessional, Features: []license.Feature{license.FeatureCore, license.FeatureLua},
		IssuedAt: now.Add(-time.Hour), NotBefore: now.Add(-time.Hour), ExpiresAt: now.Add(365 * 24 * time.Hour),
		Binding: license.Binding{Type: "installation_only", InstallationID: f.s.Lic.InstallationID()}}, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Lic.Import(raw, "admin"); err != nil {
		t.Fatal(err)
	}
	hash, err := crypto.HashPassword("Import@2026!test")
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Username: "admin2", Role: store.RoleAdmin, Source: "local", PasswordHash: hash, PasswordChangedAt: &now}
	if err := query.Use(f.s.Store.DB).User.WithContext(context.Background()).Create(u); err != nil {
		t.Fatal(err)
	}
	login := f.json(t, "", http.MethodPost, "/auth/login", map[string]string{"username": "admin2", "password": "Import@2026!test"})
	requireStatus(t, login, http.StatusOK, "")
	for _, c := range login.Result().Cookies() {
		if c.Name == cookieName {
			f.cookies["admin2"] = c
		}
	}
	if err := f.s.Deploy.SetManagementMode(f.group, deploy.ManagementModeLegacy, "admin", ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("events {}\n# secret-token\n")
	if err := tw.WriteHeader(&tar.Header{Name: "./syedge.conf", Size: int64(len(content)), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	body, ct := uploadBody(t, f.group, "legacy.tar.gz", buf.Bytes())
	path := fmt.Sprintf("/node-groups/%d/legacy-requests", f.group)
	requireStatus(t, f.request("operator", http.MethodPost, path, body, ct, true), http.StatusForbidden, "")
	w := f.request("admin", http.MethodPost, path, body, ct, true)
	requireStatus(t, w, http.StatusOK, "")
	var req store.ReleaseRequest
	if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/release-requests/%d", req.ID)
	requireStatus(t, f.request("operator", http.MethodGet, base+"/legacy-bundle", nil, "", true), http.StatusForbidden, "")
	w = f.request("admin2", http.MethodGet, base+"/legacy-bundle", nil, "", true)
	requireStatus(t, w, http.StatusOK, "")
	if w.Header().Get("Cache-Control") != "no-store" || bytes.Contains(w.Body.Bytes(), []byte("syedge-legacy-v1:")) {
		t.Fatal("review archive was not decrypted or cache control is missing")
	}
	requireStatus(t, f.json(t, "releaser", http.MethodPost, base+"/approve", nil), http.StatusForbidden, "LEGACY_ADMIN_REQUIRED")
	requireStatus(t, f.json(t, "operator", http.MethodPost, base+"/legacy-approve", nil), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, base+"/legacy-approve", nil), http.StatusForbidden, "SELF_APPROVAL")
	f.setLicense(t, 10, now.Add(-24*time.Hour))
	requireStatus(t, f.json(t, "admin2", http.MethodPost, base+"/legacy-approve", nil), http.StatusLocked, "LICENSE_READ_ONLY")
	if _, err := f.s.Lic.Import(raw, "admin"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.json(t, "admin2", http.MethodPost, base+"/legacy-approve", nil), http.StatusOK, "")
}
