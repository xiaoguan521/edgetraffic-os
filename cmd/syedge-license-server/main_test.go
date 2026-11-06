package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 明文私钥工具只能用于演示 / CI 密钥：正式 kid 的 keygen 与 issue 都被拒绝。
func TestRefusesProductionKIDs(t *testing.T) {
	if os.Getenv("LICSRV_SUBPROCESS") == "1" {
		os.Args = append([]string{"syedge-license-server"}, strings.Fields(os.Getenv("LICSRV_ARGS"))...)
		main()
		return
	}
	run := func(args string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestRefusesProductionKIDs")
		cmd.Env = append(os.Environ(), "LICSRV_SUBPROCESS=1", "LICSRV_ARGS="+args)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	dir := t.TempDir()
	if out, err := run("keygen -kid etos-prod-2027 -out " + dir); err == nil || !strings.Contains(out, "syedge-license-signer") {
		t.Fatalf("production keygen allowed: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "etos-prod-2027.key")); err == nil {
		t.Fatal("production key file written")
	}
	if out, err := run("keygen -kid etos-demo-2027 -out " + dir); err != nil {
		t.Fatalf("demo keygen: %v %s", err, out)
	}
	if out, err := run("issue -key " + filepath.Join(dir, "etos-demo-2027.key") + " -kid etos-prod-2027 -customer x -installation-id inst_abc -binding installation_only"); err == nil || !strings.Contains(out, "syedge-license-signer") {
		t.Fatalf("issuing with a production kid allowed: %v %s", err, out)
	}
}
