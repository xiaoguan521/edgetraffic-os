package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

func TestConsoleConfigFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "console.env")
	if err := os.WriteFile(file, []byte("ETOS_LISTEN=127.0.0.1:10443\nETOS_DATA=data\nETOS_TLS_AUTO=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ETOS_LISTEN", "127.0.0.1:20443")
	t.Setenv("ETOS_TLS_AUTO", "1")
	o, _, err := parseOptions([]string{"--config", file, "-listen", "127.0.0.1:30443"})
	if err != nil {
		t.Fatal(err)
	}
	if o.listen != "127.0.0.1:30443" || o.dataDir != filepath.Join(dir, "data") || o.tlsAuto {
		t.Fatalf("options = %+v", o)
	}
	o, _, err = parseOptions([]string{"-c", file})
	if err != nil || o.listen != "127.0.0.1:10443" {
		t.Fatalf("file did not override environment: %+v %v", o, err)
	}
	o, _, err = parseOptions(nil)
	if err != nil || o.listen != "127.0.0.1:20443" || !o.tlsAuto {
		t.Fatalf("existing environment behavior changed: %+v %v", o, err)
	}
}

func TestConsoleConfigProcessHelper(t *testing.T) {
	if os.Getenv("SYEDGE_CONSOLE_CONFIG_HELPER") != "1" {
		return
	}
	os.Args = []string{brand.Console(), "--config", os.Getenv("SYEDGE_CONSOLE_CONFIG_FILE")}
	main()
}

func TestConsoleStartsFromSpecifiedConfig(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "console.env")
	body := fmt.Sprintf("ETOS_LISTEN=%s\nETOS_GRPC_LISTEN=127.0.0.1:0\nETOS_DATA=runtime-data\nETOS_TLS_AUTO=0\n", addr)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConsoleConfigProcessHelper$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "ETOS_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "SYEDGE_CONSOLE_CONFIG_HELPER=1", "SYEDGE_CONSOLE_CONFIG_FILE="+file)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			if err := <-done; err != nil {
				t.Logf("console cleanup: %v", err)
			}
		}
	}()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for !ready {
		select {
		case err := <-done:
			finished = true
			t.Fatalf("console exited before starting: %v\n%s", err, output.String())
		case <-ctx.Done():
			t.Fatal("console did not start with the supplied configuration")
		case <-time.After(100 * time.Millisecond):
			resp, err := client.Get("http://" + addr + "/api/v1/setup/status")
			if err == nil {
				_ = resp.Body.Close()
				ready = resp.StatusCode == http.StatusOK
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime-data", "console.db")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		finished = true
		t.Fatalf("console shutdown: %v\n%s", err, output.String())
	}
	finished = true
}

func TestBackupUsesConsoleConfigFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "console.env")
	if err := os.WriteFile(file, []byte("ETOS_DATA=data\nETOS_DB=state.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	if err := fs.Parse([]string{"--config", file, "-data", "/override"}); err != nil {
		t.Fatal(err)
	}
	if err := c.applyConfig(fs); err != nil {
		t.Fatal(err)
	}
	if *c.dataDir != "/override" || *c.dsn != filepath.Join(dir, "state.db") {
		t.Fatalf("data=%q db=%q", *c.dataDir, *c.dsn)
	}
}

func TestConsoleRejectsBadConfigBeforeStarting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "console.env")
	if err := os.WriteFile(file, []byte("ETOS_TLS_AUTO=invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseOptions([]string{"--config", file}); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}

func TestEmptyConsoleTLSAutoOverridesEnvironment(t *testing.T) {
	file := filepath.Join(t.TempDir(), "console.env")
	if err := os.WriteFile(file, []byte("ETOS_TLS_AUTO=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ETOS_TLS_AUTO", "1")
	o, _, err := parseOptions([]string{"--config", file})
	if err != nil || o.tlsAuto {
		t.Fatalf("empty TLS setting = %t, %v", o.tlsAuto, err)
	}
}

func TestConsoleGlobalConfigBeforeBackup(t *testing.T) {
	for _, args := range [][]string{
		{"--config", "/tmp/console.env", "backup", "-out", "backup.tar"},
		{"-c=/tmp/console.env", "backup", "-out", "backup.tar"},
	} {
		got, ok := backupInvocation(args)
		if !ok || len(got) != 5 || got[0] != "backup" || got[1] != "--config" || got[2] != "/tmp/console.env" || got[3] != "-out" {
			t.Fatalf("backup arguments %v became %v, %t", args, got, ok)
		}
	}
	if _, _, err := parseOptions([]string{"unexpected"}); err == nil {
		t.Fatal("unexpected positional command was accepted")
	}
}

func TestPackagedConsoleConfigCanBeRead(t *testing.T) {
	o, _, err := parseOptions([]string{"--config", filepath.Join("..", "..", "deploy", "nfpm", "console.env")})
	if err != nil || o.listen != "127.0.0.1:8443" || o.dataDir != "/var/lib/"+brand.Console() || !o.tlsAuto {
		t.Fatalf("packaged configuration: %+v, %v", o, err)
	}
}
