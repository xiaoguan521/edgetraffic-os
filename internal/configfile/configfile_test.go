package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "console.env")
	body := "# comment\nETOS_LISTEN=127.0.0.1:8443\nETOS_DATA=../data\nETOS_DB='literal # value'\nETOS_TLS_CERT=\"certs/server.crt\"\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file, []string{"ETOS_LISTEN", "ETOS_DATA", "ETOS_DB", "ETOS_TLS_CERT"})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Value("ETOS_DB"); !ok || v != "literal # value" {
		t.Fatalf("quoted value = %q, %t", v, ok)
	}
	if v, ok := c.Path("ETOS_DATA"); !ok || v != filepath.Clean(filepath.Join(dir, "../data")) {
		t.Fatalf("data path = %q, %t", v, ok)
	}
	if v, ok := c.Path("ETOS_TLS_CERT"); !ok || v != filepath.Join(dir, "certs/server.crt") {
		t.Fatalf("certificate = %q, %t", v, ok)
	}
	if _, ok := c.Value("MISSING"); ok {
		t.Fatal("missing key was present")
	}
}

func TestConfigurationValuesAreNotExecuted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	file := filepath.Join(dir, "config.env")
	value := "$(touch " + marker + ")"
	if err := os.WriteFile(file, []byte("ETOS_DB="+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file, []string{"ETOS_DB"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Value("ETOS_DB"); got != value {
		t.Fatalf("value = %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("configuration was executed as shell")
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":      "ETOS_LISTEN=x\nETOS_LITSEN=y\n",
		"duplicate":    "ETOS_LISTEN=x\nETOS_LISTEN=y\n",
		"syntax":       "ETOS_LISTEN\n",
		"unterminated": "ETOS_LISTEN=\"secret\n",
		"nul":          "ETOS_LISTEN=x\x00y\n",
		"oversize":     "ETOS_LISTEN=" + strings.Repeat("a", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.env")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(file, []string{"ETOS_LISTEN"}); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing"), []string{"ETOS_LISTEN"}); err == nil {
		t.Fatal("missing configuration was accepted")
	}
}
