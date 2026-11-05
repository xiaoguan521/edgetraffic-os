package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

type staticEntry struct {
	name, body, link string // link != "" makes a symlink
	hard             bool   // tar hard link instead of symlink
}

func staticZip(t *testing.T, entries ...staticEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		body := e.body
		if e.link != "" {
			h.SetMode(fs.ModeSymlink | 0o777)
			body = e.link
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func staticTarGz(t *testing.T, entries ...staticEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
			if e.hard {
				h.Typeflag = tar.TypeLink
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
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

func (f *importHTTP) uploadStatic(t *testing.T, role, file string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("group_id", fmt.Sprint(f.group))
	_ = mw.WriteField("name", "bundle")
	p, err := mw.CreateFormFile("file", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return f.request(role, http.MethodPost, "/static-bundles", b.Bytes(), mw.FormDataContentType(), true)
}

// STATIC-04 regression through the real HTTP route, middleware and database:
// unsafe archive entries and bundles without an entry page are rejected and
// never stored.
func TestStaticHTTPRejectsUnsafeBundles(t *testing.T) {
	f := newImportHTTP(t)
	idx := staticEntry{name: "index.html", body: "<h1>ok</h1>"}
	for _, tc := range []struct {
		name, file string
		data       []byte
		msg        string
	}{
		{"dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: "../escape.conf", body: "x"}), "越界"},
		{"nested dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: "js/../../escape.conf", body: "x"}), "越界"},
		{"backslash dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: `..\escape.conf`, body: "x"}), "越界"},
		{"absolute", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "/etc/escape.conf", body: "x"}), "绝对路径"},
		{"drive letter", "a.zip", staticZip(t, idx, staticEntry{name: `C:\escape.conf`, body: "x"}), "绝对路径"},
		{"tar symlink", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "passwd", link: "/etc/passwd"}), "链接"},
		{"tar hard link", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "passwd", link: "/etc/passwd", hard: true}), "链接"},
		{"zip symlink", "a.zip", staticZip(t, idx, staticEntry{name: "passwd", link: "/etc/passwd"}), "链接"},
		{"missing index", "a.zip", staticZip(t, staticEntry{name: "dist/js/app.js", body: "1"}), "index.html"},
		{"index only in subdir", "a.zip", staticZip(t, staticEntry{name: "a/index.html", body: "x"}, staticEntry{name: "b/app.js", body: "1"}), "index.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.uploadStatic(t, "operator", tc.file, tc.data)
			requireStatus(t, w, http.StatusBadRequest, "STATIC_REJECTED")
			if !strings.Contains(w.Body.String(), tc.msg) {
				t.Fatalf("error does not mention %q: %s", tc.msg, w.Body)
			}
		})
	}

	// Insufficient role is refused before the archive is inspected.
	requireStatus(t, f.uploadStatic(t, "viewer", "ok.zip", staticZip(t, idx)), http.StatusForbidden, "")

	// A well-formed bundle under a single top-level directory is accepted.
	w := f.uploadStatic(t, "operator", "ok.tar.gz", staticTarGz(t,
		staticEntry{name: "dist/index.html", body: "<h1>ok</h1>"}, staticEntry{name: "dist/js/app.js", body: "1"}))
	requireStatus(t, w, http.StatusOK, "")
	var row store.StaticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil || row.Files != 2 {
		t.Fatalf("accepted bundle: %v %s", err, w.Body)
	}

	// Only the accepted bundle was persisted.
	lw := f.request("operator", http.MethodGet, "/static-bundles", nil, "", false)
	requireStatus(t, lw, http.StatusOK, "")
	var rows []store.StaticBundle
	if err := json.Unmarshal(lw.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].ID != row.ID {
		t.Fatalf("stored bundles = %s", lw.Body)
	}
}
