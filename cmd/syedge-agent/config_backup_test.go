package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitConfigBackupIncludesSelectedDependencies(t *testing.T) {
	base := t.TempDir()
	config := filepath.Join(base, "custom", "syedge.conf")
	lua := filepath.Join(base, "external", "script.lua")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(lua), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("include custom.conf;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lua, []byte("return true"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(config), "linked.lua")
	if err := os.Symlink(lua, link); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "backup.tar.gz")
	if err := backupConfig([]string{"--config", config, "--source", filepath.Dir(config), "--source", filepath.Dir(lua), "--output", out}, io.Discard); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions: %v %v", fi, err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	tr := tar.NewReader(z)
	found := map[string]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = h.Typeflag
	}
	for _, name := range []string{config, lua, link} {
		entry := "rootfs/" + strings.TrimPrefix(filepath.ToSlash(name), "/")
		if _, ok := found[entry]; !ok {
			t.Errorf("backup missing %s", entry)
		}
	}
	if found["rootfs/"+strings.TrimPrefix(filepath.ToSlash(link), "/")] != tar.TypeSymlink {
		t.Fatal("backup did not preserve the symbolic link")
	}
}

func TestExplicitConfigBackupRefusesMissingLinkTargetAndOverwrite(t *testing.T) {
	base := t.TempDir()
	config := filepath.Join(base, "conf", "syedge.conf")
	target := filepath.Join(base, "other", "secret.lua")
	for _, dir := range []string{filepath.Dir(config), filepath.Dir(target)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(config, []byte("events {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(filepath.Dir(config), "external.lua")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "backup.tar.gz")
	err := backupConfig([]string{"--config", config, "--source", filepath.Dir(config), "--output", out}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("missing linked target accepted: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("incomplete archive left behind: %v", err)
	}
	if err := os.WriteFile(out, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = backupConfig([]string{"--config", config, "--source", filepath.Dir(config), "--source", target, "--output", out}, io.Discard)
	if err == nil {
		t.Fatal("existing backup was overwritten")
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "do not overwrite" {
		t.Fatalf("existing backup changed: %q %v", got, err)
	}
}

func TestConfigBackupPreservesCurrentSymlinkWithoutShadowDirectory(t *testing.T) {
	base := t.TempDir()
	conf := filepath.Join(base, "conf")
	rel := filepath.Join(conf, "releases", "r000000")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "syedge.conf"), []byte("events {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/r000000", filepath.Join(conf, "current")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "backup.tar.gz")
	if err := backupConfig([]string{"--config", filepath.Join(conf, "current", "syedge.conf"), "--source", conf, "--output", out}, io.Discard); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	tr := tar.NewReader(z)
	found := map[string]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = h.Typeflag
	}
	name := func(p string) string { return "rootfs/" + strings.TrimPrefix(filepath.ToSlash(p), "/") }
	if found[name(filepath.Join(conf, "current"))] != tar.TypeSymlink {
		t.Fatal("current link was not preserved")
	}
	if _, ok := found[name(filepath.Join(conf, "current", "syedge.conf"))]; ok {
		t.Fatal("archive contains a file under the symlink path")
	}
	if _, ok := found[name(filepath.Join(rel, "syedge.conf"))]; !ok {
		t.Fatal("archive is missing the actual configuration")
	}
}
