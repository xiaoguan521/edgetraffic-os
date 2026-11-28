package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
)

func TestAlertSilenceSuppressesNotify(t *testing.T) {
	st := storetest.Open(t)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc := &Service{Store: st, Now: func() time.Time { return now }}

	if err := svc.SetSilence(now.Add(30*time.Minute), "夜间升级"); err != nil {
		t.Fatalf("SetSilence failed: %v", err)
	}
	until, reason := svc.Silence()
	if until.IsZero() || reason != "夜间升级" {
		t.Fatalf("unexpected silence: until=%v reason=%q", until, reason)
	}
	res := svc.notify(&store.AlertRule{ChannelIDs: "1"}, Message{Title: "test"})
	if !strings.Contains(res, "已静默") || !strings.Contains(res, "夜间升级") {
		t.Fatalf("expected silenced notify result, got %q", res)
	}

	if err := svc.SetSilence(time.Time{}, ""); err != nil {
		t.Fatalf("clear silence failed: %v", err)
	}
	if u, _ := svc.Silence(); !u.IsZero() {
		t.Fatalf("expected cleared silence, got %v", u)
	}
}
