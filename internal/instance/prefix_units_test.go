package instance

import (
	"os"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// 软件包单元的路径都以编译期默认根目录为前缀，默认根目录（不在 /home 下）不需要改写。
func TestCompiledRootUnitIsUnchanged(t *testing.T) {
	root := layout.DefaultSystemRoot()
	if strings.HasPrefix(root, "/home/") {
		t.Skip("a /home compiled prefix needs the home sandbox rewrite")
	}
	src := "[Service]\nReadWritePaths=" + root + "/agent " + root + "/logs " + root + "/cache\nExecStart=" + root + "/sbin/syedge -p " + root + "\n"
	if got := prefixUnit(src, root, true); got != src {
		t.Errorf("default root unit rewritten:\n%s", got)
	}
}

func TestPrefixUnitCustomRootIsIdempotent(t *testing.T) {
	src := `[Unit]
ConditionPathExists=/opt/syedge/instances/%i/instance.json

[Service]
ReadWritePaths=/opt/syedge/instances/%i/conf /opt/syedge/instances/%i/agent /run/syedge-agent-%i /opt/syedge/instances/%i/run /opt/syedge/instances/%i/logs /opt/syedge/instances/%i/cache -/opt/syedge/versions /opt/syedge/sbin
ExecStart=/usr/bin/syedge-agent run -instance %i
ExecStartPre=/opt/syedge/sbin/syedge -p /opt/syedge/instances/%i -t -q
ExecReload=/opt/syedge/sbin/syedge -p /opt/syedge/instances/%i -s reload
`
	got := prefixUnit(src, "/data/syedge", true)
	if !strings.Contains(got, "ConditionPathExists=/data/syedge/instances/%i/instance.json") {
		t.Error("condition path was not rebased")
	}
	if !strings.Contains(got, "ReadWritePaths=/data/syedge/instances/%i/conf /data/syedge/instances/%i/agent") {
		t.Error("read-write paths were not rebased")
	}
	// 数据面只带 -p，不追加 -c、-e
	if !strings.Contains(got, "ExecStartPre=/data/syedge/sbin/syedge -p /data/syedge/instances/%i -t -q\n") {
		t.Errorf("custom ExecStartPre = %q", got)
	}
	if !strings.Contains(got, "ExecReload=/data/syedge/sbin/syedge -p /data/syedge/instances/%i -s reload\n") {
		t.Error("custom ExecReload was not rebased")
	}
	if strings.Contains(got, " -e ") || strings.Contains(got, " -c ") {
		t.Errorf("unit must not carry -c/-e:\n%s", got)
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
