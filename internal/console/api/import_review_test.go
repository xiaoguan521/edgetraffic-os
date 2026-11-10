package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestImportReviewTracksDraftAndApproval(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	path := fmt.Sprintf("/imports/%d/review", row.ID)
	requireStatus(t, f.json(t, "operator", http.MethodGet, path, nil), 409, "NOT_APPLIED")
	var applied ApplyResult
	w := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	var review struct {
		Sites []struct {
			ID              uint   `json:"id"`
			SourceName      string `json:"source_name"`
			Valid           bool   `json:"valid"`
			Missing         bool   `json:"missing"`
			ValidationError string `json:"validation_error"`
		} `json:"sites"`
		Snippets []struct {
			Status string `json:"status"`
		} `json:"snippets"`
	}
	check := func() {
		t.Helper()
		w := f.json(t, "operator", http.MethodGet, path, nil)
		requireStatus(t, w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if len(review.Sites) != 1 || review.Sites[0].ID == 0 || review.Sites[0].SourceName == "" || !review.Sites[0].Valid || len(review.Snippets) != 1 || review.Snippets[0].Status != "pending" {
		t.Fatalf("unexpected review: %+v", review)
	}
	requireStatus(t, f.json(t, "viewer", http.MethodGet, path, nil), 403, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, fmt.Sprintf("/snippets/%d/approve", applied.Snippets[0]), map[string]any{}), 200, "")
	check()
	if review.Snippets[0].Status != "approved" {
		t.Fatalf("approval not reflected: %+v", review.Snippets)
	}
	requireStatus(t, f.json(t, "operator", http.MethodDelete, fmt.Sprintf("/sites/%d", review.Sites[0].ID), nil), 204, "")
	check()
	if !review.Sites[0].Missing || review.Sites[0].Valid {
		t.Fatalf("deleted site was reported as valid: %+v", review.Sites[0])
	}
}

func TestImportApplyPreservesWebSocket(t *testing.T) {
	f := newImportHTTP(t)
	up := &store.Upstream{GroupID: f.group, Name: "backend", Spec: `{"name":"backend","servers":[{"addr":"10.0.2.2:8080"}]}`}
	if err := f.s.q().Upstream.WithContext(context.Background()).Create(up); err != nil {
		t.Fatal(err)
	}
	src := `events {} http { upstream backend { server 10.0.1.1:8080; } server { listen 80; server_name socket.example.com; location / { proxy_pass http://backend; proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade"; } } }`
	row := decodeImport(t, f.upload(t, "operator", "web.conf", []byte(src)))
	w := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, 200, "")
	var applied struct {
		Sites []struct {
			ID uint `json:"id"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &applied); err != nil || len(applied.Sites) != 1 || applied.Sites[0].ID == 0 {
		t.Fatalf("missing imported site identity: %s (%v)", w.Body.String(), err)
	}
	w = f.json(t, "operator", http.MethodGet, fmt.Sprintf("/sites/%d", applied.Sites[0].ID), nil)
	requireStatus(t, w, 200, "")
	var view struct {
		Spec render.Site `json:"spec"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Spec.Routes) != 1 || view.Spec.Routes[0].Proxy == nil || !view.Spec.Routes[0].Proxy.WebSocket || view.Spec.Routes[0].Proxy.Upstream != "backend-2" {
		t.Fatalf("WebSocket lost while applying import: %s (%v)", w.Body.String(), err)
	}
}

func TestImportReviewChecksDisabledDraftResources(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	w := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	var applied ApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &applied); err != nil || len(applied.Sites) != 1 {
		t.Fatalf("apply: %s (%v)", w.Body.String(), err)
	}
	id := applied.Sites[0].ID
	spec := render.Site{Name: applied.Sites[0].Name, Domains: []string{"imported.example.com"}, Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "static", Static: &render.StaticSpec{BundleID: 999999}}}}
	w = f.json(t, "operator", http.MethodPut, fmt.Sprintf("/sites/%d", id), map[string]any{"version": 1, "enabled": false, "spec": spec})
	requireStatus(t, w, 200, "")
	w = f.json(t, "operator", http.MethodGet, fmt.Sprintf("/imports/%d/review", row.ID), nil)
	requireStatus(t, w, 200, "")
	var review struct {
		Sites []struct {
			Valid bool   `json:"valid"`
			Error string `json:"validation_error"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil || len(review.Sites) != 1 || review.Sites[0].Valid || !strings.Contains(review.Sites[0].Error, "静态资源包") {
		t.Fatalf("missing bundle was not reported: %s (%v)", w.Body.String(), err)
	}
	spec.Routes[0] = render.Route{Path: "/", Match: "prefix", Mode: "deny"}
	w = f.json(t, "operator", http.MethodPut, fmt.Sprintf("/sites/%d", id), map[string]any{"version": 2, "enabled": false, "spec": spec})
	requireStatus(t, w, 200, "")
	w = f.json(t, "operator", http.MethodGet, fmt.Sprintf("/imports/%d/review", row.ID), nil)
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil || len(review.Sites) != 1 || !review.Sites[0].Valid {
		t.Fatalf("fixed draft did not validate: %s (%v)", w.Body.String(), err)
	}
}

func TestImportReviewDoesNotGuessLegacySiteIdentity(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	requireStatus(t, f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), 200, "")
	q := f.s.q().ConfigImport
	legacy := `{"upstreams":[],"certificates":[],"sites":[{"name":"imported","enabled":true}],"snippets":[],"errors":[]}`
	if _, err := q.WithContext(context.Background()).Where(q.ID.Eq(row.ID)).Update(q.Result, legacy); err != nil {
		t.Fatal(err)
	}
	w := f.json(t, "operator", http.MethodGet, fmt.Sprintf("/imports/%d/review", row.ID), nil)
	requireStatus(t, w, 200, "")
	var review struct {
		Sites []struct {
			IdentityUnverified bool `json:"identity_unverified"`
			Valid              bool `json:"valid"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil || len(review.Sites) != 1 || !review.Sites[0].IdentityUnverified || review.Sites[0].Valid {
		t.Fatalf("legacy record identity guessed: %s (%v)", w.Body.String(), err)
	}
}
