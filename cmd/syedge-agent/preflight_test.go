package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

func TestPreflightDetectsStandaloneConfigAndLua(t *testing.T) {
	root := t.TempDir()
	confDir := filepath.Join(root, "conf", "releases", "r000000")
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "r000000"), filepath.Join(root, "conf", "current")); err != nil {
		t.Fatal(err)
	}
	instJSON, _ := json.Marshal(map[string]any{"mode": "user", "root": root, "http_port": 8080, "https_port": 8443, "status_port": 18081})
	if err := os.WriteFile(filepath.Join(root, "instance.json"), instJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	mainConf := "http {\n    include conf.d/*.conf;\n    init_by_lua_file lua/init.lua;\n}\n"
	if err := os.WriteFile(filepath.Join(confDir, brand.ConfName()), []byte(mainConf), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := preflightMain([]string{"-root", root, "-json"}, &buf); err != nil {
		t.Fatalf("preflightMain failed: %v", err)
	}
	var rep preflightReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, buf.String())
	}
	if rep.Mode != "user" {
		t.Fatalf("mode = %q, want user", rep.Mode)
	}
	if !rep.HasCustomConfig {
		t.Fatalf("expected HasCustomConfig=true when manifest missing")
	}
	if !rep.HasNativeLua {
		t.Fatalf("expected HasNativeLua=true")
	}
	if rep.RecommendedMode != "legacy" {
		t.Fatalf("recommended = %q, want legacy", rep.RecommendedMode)
	}

	var human bytes.Buffer
	if err := preflightMain([]string{"-root", root}, &human); err != nil {
		t.Fatalf("preflightMain human failed: %v", err)
	}
	if !strings.Contains(human.String(), "控制台实例档案对照表") {
		t.Fatalf("expected human table, got:\n%s", human.String())
	}
}

func TestExportConfigPackagesIncludesAndChecksRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lua"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, brand.ConfName())
	if err := os.WriteFile(mainPath, []byte("http {\n  include conf.d/*.conf;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf.d", "app.conf"), []byte("server {\n  listen 80;\n  access_by_lua_file lua/check.lua;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lua", "check.lua"), []byte("return\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outTar := filepath.Join(t.TempDir(), "export.tar.gz")
	var buf bytes.Buffer
	if err := exportConfig([]string{"--config", mainPath, "--output", outTar}, &buf); err != nil {
		t.Fatalf("exportConfig failed: %v", err)
	}

	f, err := os.Open(outTar)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = true
	}
	for _, want := range []string{brand.ConfName(), "conf.d/app.conf", "lua/check.lua"} {
		if !got[want] {
			t.Errorf("archive missing %q, got %v", want, got)
		}
	}
}

func TestBuildConfigArchiveFiltersPrivateKeysByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, brand.ConfName())
	if err := os.WriteFile(mainPath, []byte("http {\n  include conf.d/*.conf;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	siteConf := "server {\n  listen 443 ssl;\n  server_name demo.example.com;\n  ssl_certificate certs/demo.crt;\n  ssl_certificate_key certs/demo.key;\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "conf.d", "demo.conf"), []byte(siteConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "certs", "demo.crt"), []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "certs", "demo.key"), []byte("-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readNames := func(archive []byte) map[string]bool {
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gz.Close() }()
		tr := tar.NewReader(gz)
		m := map[string]bool{}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			m[h.Name] = true
		}
		return m
	}

	var logBuf bytes.Buffer
	archNoKey, err := buildConfigArchive(mainPath, false, &logBuf)
	if err != nil {
		t.Fatalf("buildConfigArchive(includeKeys=false) failed: %v", err)
	}
	gotNoKey := readNames(archNoKey)
	if gotNoKey["certs/demo.key"] {
		t.Fatalf("private key must be filtered when includeKeys=false")
	}
	if !gotNoKey["conf.d/demo.conf"] || !gotNoKey["certs/demo.crt"] {
		t.Fatalf("expected conf and cert in archive, got %v", gotNoKey)
	}

	archWithKey, err := buildConfigArchive(mainPath, true, nil)
	if err != nil {
		t.Fatalf("buildConfigArchive(includeKeys=true) failed: %v", err)
	}
	gotWithKey := readNames(archWithKey)
	if !gotWithKey["certs/demo.key"] {
		t.Fatalf("expected private key when includeKeys=true, got %v", gotWithKey)
	}
}
