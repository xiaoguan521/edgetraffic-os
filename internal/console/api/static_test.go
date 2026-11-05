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

func (f *importHTTP) uploadStatic(t *testing.T, role, file string, data []byte, extra ...string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("group_id", fmt.Sprint(f.group))
	_ = mw.WriteField("name", "bundle")
	for i := 0; i+1 < len(extra); i += 2 {
		_ = mw.WriteField(extra[i], extra[i+1])
	}
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
		code       string
	}{
		{"dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: "../escape.conf", body: "x"}), "越界"},
		{"nested dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: "js/../../escape.conf", body: "x"}), "越界"},
		{"backslash dot-dot", "a.zip", staticZip(t, idx, staticEntry{name: `..\escape.conf`, body: "x"}), "越界"},
		{"absolute", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "/etc/escape.conf", body: "x"}), "绝对路径"},
		{"drive letter", "a.zip", staticZip(t, idx, staticEntry{name: `C:\escape.conf`, body: "x"}), "绝对路径"},
		{"tar symlink", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "passwd", link: "/etc/passwd"}), "链接"},
		{"tar hard link", "a.tar.gz", staticTarGz(t, idx, staticEntry{name: "passwd", link: "/etc/passwd", hard: true}), "链接"},
		{"zip symlink", "a.zip", staticZip(t, idx, staticEntry{name: "passwd", link: "/etc/passwd"}), "链接"},
		{"missing index", "a.zip", staticZip(t, staticEntry{name: "dist/js/app.js", body: "1"}), "index.html", "STATIC_NO_INDEX"},
		{"index only in subdir", "a.zip", staticZip(t, staticEntry{name: "a/index.html", body: "x"}, staticEntry{name: "b/app.js", body: "1"}), "index.html", "STATIC_NO_INDEX"},
		{"only hidden files", "a.zip", staticZip(t, staticEntry{name: ".git/config", body: "x"}, staticEntry{name: ".DS_Store", body: "x"}), "没有文件", ""},
		{"nested well-known", "a.zip", staticZip(t, idx, staticEntry{name: "js/.well-known/x", body: "x"}, staticEntry{name: "js/app.js", body: "1"}), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.uploadStatic(t, "operator", tc.file, tc.data)
			if tc.msg == "" {
				// 不是拒绝用例：嵌套的 .well-known/ 只是被跳过
				requireStatus(t, w, http.StatusOK, "")
				var row store.StaticBundle
				if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil || row.Files != 2 {
					t.Fatalf("nested .well-known must be skipped: %v %s", err, w.Body)
				}
				f.request("operator", http.MethodDelete, fmt.Sprintf("/static-bundles/%d", row.ID), nil, "", true)
				return
			}
			code := tc.code
			if code == "" {
				code = "STATIC_REJECTED"
			}
			requireStatus(t, w, http.StatusBadRequest, code)
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

// 隐藏内容（.git/、.DS_Store、__MACOSX/）跳过而不是整包拒绝；根目录 .well-known/ 保留，
// 其中 acme-challenge/ 跳过；allow_no_index=1 允许没有入口页的纯资源包。
func TestStaticHTTPHiddenWellKnownAndNoIndex(t *testing.T) {
	f := newImportHTTP(t)

	w := f.uploadStatic(t, "operator", "site.zip", staticZip(t,
		staticEntry{name: "dist/index.html", body: "<h1>ok</h1>"},
		staticEntry{name: "dist/.git/config", body: "[core]"},
		staticEntry{name: "dist/.git/HEAD", body: "ref: refs/heads/main"},
		staticEntry{name: "dist/.env", body: "SECRET=1"},
		staticEntry{name: "dist/.well-known/security.txt", body: "Contact: mailto:sec@example.com"},
		staticEntry{name: "dist/.well-known/acme-challenge/token", body: "x"},
		staticEntry{name: "__MACOSX/dist/._index.html", body: "x"},
		staticEntry{name: ".DS_Store", body: "x"},
	))
	requireStatus(t, w, http.StatusOK, "")
	var row store.StaticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	// 只剩 index.html 与 .well-known/security.txt（dist/ 顶层被去掉）
	if row.Files != 2 {
		t.Fatalf("files = %d, want 2: %s", row.Files, w.Body)
	}

	// 没有入口页：默认拒绝（专用错误码，前端据此询问），勾选后接受
	assets := staticTarGz(t, staticEntry{name: "files/a.pdf", body: "%PDF"}, staticEntry{name: "files/img/b.png", body: "png"})
	requireStatus(t, f.uploadStatic(t, "operator", "assets.tar.gz", assets), http.StatusBadRequest, "STATIC_NO_INDEX")
	w = f.uploadStatic(t, "operator", "assets.tar.gz", assets, "allow_no_index", "1")
	requireStatus(t, w, http.StatusOK, "")
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil || row.Files != 2 {
		t.Fatalf("asset-only bundle: %v %s", err, w.Body)
	}

	// 勾选不放宽任何安全检查
	requireStatus(t, f.uploadStatic(t, "operator", "evil.zip",
		staticZip(t, staticEntry{name: "../escape.conf", body: "x"}), "allow_no_index", "1"), http.StatusBadRequest, "STATIC_REJECTED")
}
