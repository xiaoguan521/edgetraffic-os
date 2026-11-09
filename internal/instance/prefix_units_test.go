package instance

import (
	"strings"
	"testing"
)

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
	got := prefixUnit(src, "/data/syedge")
	if !strings.Contains(got, "ConditionPathExists=/data/syedge/instances/%i/instance.json") {
		t.Error("condition path was not rebased")
	}
	if !strings.Contains(got, "ReadWritePaths=/data/syedge/instances/%i/conf /data/syedge/instances/%i/agent") {
		t.Error("read-write paths were not rebased")
	}
	if strings.Contains(got, "StateDirectory=") {
		t.Error("StateDirectory must be removed for a custom root")
	}
	if !strings.Contains(got, "ExecStartPre=/data/syedge/sbin/syedge -t -q -c /data/syedge/instances/%i/conf/current/syedge.conf -e /data/syedge/instances/%i/logs/error.log") {
		t.Error("custom ExecStartPre was not completed")
	}
	if !strings.Contains(got, "ExecReload=/data/syedge/sbin/syedge -s reload -c /data/syedge/conf/current/syedge.conf -e /data/syedge/logs/error.log") {
		t.Error("custom ExecReload was not completed")
	}
	if again := prefixUnit(got, "/data/syedge"); again != got {
		t.Error("prefixUnit is not idempotent")
	}
}
