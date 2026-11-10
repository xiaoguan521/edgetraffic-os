package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

func TestInstanceConfigDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "instance.env")
	body := brand.Env("HTTP") + "=18080\n" + brand.Env("HTTPS") + "=18443\n" + brand.Env("STATUS") + "=8082\n" + brand.Env("RESOURCE_MODE") + "=independent\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := parseInvocation([]string{"--config", file, "add", "blue", "--http", "19080"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := parseAddOptions(inv.rest, inv.defaults)
	if err != nil {
		t.Fatal(err)
	}
	if inv.cmd != "add" || o.Profile.Instance != "blue" || o.Profile.HTTPPort != 19080 || o.Profile.HTTPSPort != 18443 || o.Profile.StatusPort != 8082 || o.Profile.ResourceMode != "independent" {
		t.Fatalf("invocation=%+v, options=%+v", inv, o)
	}
	inv, err = parseInvocation([]string{"add", "blue", "--config", file, "--http", "19080"})
	if err != nil {
		t.Fatal(err)
	}
	o, err = parseAddOptions(inv.rest, inv.defaults)
	if err != nil || o.Profile.HTTPSPort != 18443 || o.Profile.HTTPPort != 19080 {
		t.Fatalf("config after command: %+v, %v", o, err)
	}
}

func TestInstanceConfigDoesNotConsumeFlagValues(t *testing.T) {
	want := []string{"blue", "--token", "-c", "--http", "18080"}
	inv, err := parseInvocation(append([]string{"add"}, want...))
	if err != nil || !reflect.DeepEqual(inv.rest, want) {
		t.Fatalf("flag values changed: %v, %v", inv.rest, err)
	}
}

func TestInstanceConfigCannotBypassUserModeRestriction(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "instance.env")
	if err := os.WriteFile(file, []byte(brand.Env("ROOT")+"=user-root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := parseInvocation([]string{"-c", file, "list"})
	if err != nil || inv.root != filepath.Join(dir, "user-root") {
		t.Fatalf("root = %q, %v", inv.root, err)
	}
	var out bytes.Buffer
	if code := run([]string{"-c", file, "list"}, &out, &out, 0); code != 1 {
		t.Fatalf("root execution accepted user mode: code %d, output %s", code, out.String())
	}
}
