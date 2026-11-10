package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func TestAgentConfigProcessHelper(t *testing.T) {
	if os.Getenv("SYEDGE_TEST_AGENT_HELPER") != "1" {
		return
	}
	os.Args = []string{brand.Agent(), "--config", os.Getenv("SYEDGE_TEST_AGENT_CONFIG"), "version"}
	main()
}

func TestAgentUsesSpecifiedConfigInProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-mode agent cannot run as root")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "node")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := json.Marshal(render.Profile{Mode: "user", Root: root, HTTPPort: 8080, HTTPSPort: 8443, StatusPort: 8081})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, layout.InstanceFile), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(file, []byte(brand.Env("ROOT")+"=node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentConfigProcessHelper$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, strings.ToUpper(brand.ID())+"_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "SYEDGE_TEST_AGENT_HELPER=1", "SYEDGE_TEST_AGENT_CONFIG="+file)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), version.String("agent")) {
		t.Fatalf("agent process: %v\n%s", err, out)
	}
}

func TestAgentConfigFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent.env")
	body := brand.Env("CONSOLE") + "=console.example:9443\n" + brand.Env("INSTANCE") + "=blue\n" + brand.Env("STATE_DIR") + "=state\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brand.Env("CONSOLE"), "env.example:9443")
	o, err := parseAgentOptions("run", []string{"--config", file, "-instance", "green"})
	if err != nil {
		t.Fatal(err)
	}
	if o.server != "console.example:9443" || o.instance != "green" || o.stateDir != filepath.Join(dir, "state") {
		t.Fatalf("options = %+v", o)
	}
	o, err = parseAgentOptions("run", nil)
	if err != nil || o.server != "env.example:9443" {
		t.Fatalf("existing environment behavior changed: %+v %v", o, err)
	}
	cmd, args, err := parseAgentInvocation([]string{"--config", file, "run", "-instance", "green"})
	if err != nil || cmd != "run" {
		t.Fatalf("invocation: %q %v", cmd, err)
	}
	o, err = parseAgentOptions(cmd, args)
	if err != nil || o.server != "console.example:9443" || o.instance != "green" {
		t.Fatalf("global config: %+v %v", o, err)
	}
}
