package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func TestCompiledNonLegacyRootUsesItsOwnDirectories(t *testing.T) {
	root := layout.DefaultSystemRoot()
	if root == layout.LegacySystemRoot() {
		t.Skip("requires a non-legacy compiled prefix")
	}
	src := "[Service]\nStateDirectory=syedge-agent\nReadWritePaths=/var/lib/syedge-agent /var/log/syedge /var/cache/syedge\n"
	got := prefixUnit(src, root, true)
	for _, want := range []string{root + "/agent", root + "/logs", root + "/cache"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "StateDirectory=") {
		t.Error("legacy state directory remained")
	}
}

func TestPrefixUnitCustomRootIsIdempotent(t *testing.T) {
	src := `[Unit]
ConditionPathExists=/opt/syedge/instances/%i/instance.json

[Service]
ReadWritePaths=/opt/syedge/instances/%i/conf /var/lib/syedge-agent-%i /run/syedge-agent-%i -/run/syedge-%i /var/log/syedge-%i /var/cache/syedge-%i -/opt/syedge/versions /opt/syedge/sbin
StateDirectory=syedge-agent-%i
ExecStart=/usr/bin/syedge-agent run -instance %i
ExecStartPre=/opt/syedge/sbin/syedge -t -q -c /opt/syedge/instances/%i/conf/current/syedge.conf
ExecReload=/opt/syedge/sbin/syedge -s reload
`
	got := prefixUnit(src, "/data/syedge", true)
	if !strings.Contains(got, "ConditionPathExists=/data/syedge/instances/%i/instance.json") {
		t.Error("condition path was not rebased")
	}
	if !strings.Contains(got, "ReadWritePaths=/data/syedge/instances/%i/conf /data/syedge/instances/%i/agent") {
		t.Error("read-write paths were not rebased")
	}
	if strings.Contains(got, "StateDirectory=") {
		t.Error("StateDirectory must be removed for a custom root")
	}
	if !strings.Contains(got, "ExecStartPre=/data/syedge/sbin/syedge -t -q -c /data/syedge/instances/%i/conf/current/syedge.conf -e /data/syedge/instances/%i/logs/error.log -p /data/syedge") {
		t.Error("custom ExecStartPre was not completed")
	}
	if !strings.Contains(got, "ExecReload=/data/syedge/sbin/syedge -s reload -c /data/syedge/conf/current/syedge.conf -e /data/syedge/logs/error.log -p /data/syedge") {
		t.Error("custom ExecReload was not completed")
	}
	if again := prefixUnit(got, "/data/syedge", true); again != got {
		t.Error("prefixUnit is not idempotent")
	}

	homeSrc := src + "ProtectHome=true\n"
	if dataGot := prefixUnit(homeSrc, "/data/syedge", true); !strings.Contains(dataGot, "ProtectHome=true") {
		t.Error("ProtectHome must be kept for non-/home custom root")
	}
	homeGot := prefixUnit(homeSrc, "/home/atwasoft/node", true)
	if !strings.Contains(homeGot, "ProtectHome=tmpfs\nBindPaths=/home/atwasoft/node\n") {
		t.Errorf("home custom root must be exposed without exposing other homes:\n%s", homeGot)
	}
	if again := prefixUnit(homeGot, "/home/atwasoft/node", true); again != homeGot {
		t.Error("prefixUnit for /home custom root is not idempotent")
	}
	legacy := prefixUnit(homeSrc, "/home/atwasoft/node", false)
	if !strings.Contains(legacy, "ProtectHome=false\nInaccessibleDirectories=-/root -/run/user\n") {
		t.Errorf("legacy systemd must still hide /root and /run/user:\n%s", legacy)
	}
	if again := prefixUnit(legacy, "/home/atwasoft/node", false); again != legacy {
		t.Error("legacy prefixUnit for /home custom root is not idempotent")
	}
	if upgraded := prefixUnit(legacy, "/home/atwasoft/node", true); upgraded != homeGot {
		t.Error("systemd upgrade must replace the legacy home sandbox")
	}
}

func TestCustomLogrotatePreservesLocalChanges(t *testing.T) {
	f := newFake(t)
	if err := f.writeLogrotate("/srv/syedge"); err != nil {
		t.Fatal(err)
	}
	p := f.path("/etc/logrotate.d/syedge")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(strings.ReplaceAll(string(b), "/srv/syedge/logs/", "/var/log/local/")), []byte("# locally adjusted policy\n")...)
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.writeLogrotate("/srv/syedge"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != string(want) {
		t.Fatalf("local logrotate policy changed: %q, %v", got, err)
	}
}
