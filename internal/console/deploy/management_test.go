package deploy

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
)

func TestManagementModeDefaultsAndTransitions(t *testing.T) {
	d, g := newTestService(t)
	if got, err := d.ManagementMode(g.ID); err != nil || got != ManagementModeManaged {
		t.Fatalf("default management mode = %q, %v", got, err)
	}
	if err := d.SetManagementMode(g.ID, ManagementModeObserve, "alice", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ManagementMode(g.ID); err != nil || got != ManagementModeObserve {
		t.Fatalf("observe management mode = %q, %v", got, err)
	}
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "alice", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetManagementMode(g.ID, ManagementModeManaged, "alice", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ManagementMode(999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing group management mode = %v", err)
	}
	if err := d.SetManagementMode(g.ID, "unsafe", "alice", "127.0.0.1"); !errors.Is(err, ErrInvalidManagementMode) {
		t.Fatalf("invalid management mode = %v", err)
	}
	var n int64
	if err := d.Store.DB.WithContext(context.Background()).Table("audit_logs").Where("action = ? AND object_id = ?", "group.management_mode", fmt.Sprint(g.ID)).Count(&n).Error; err != nil || n != 3 {
		t.Fatalf("management mode audit rows = %d, %v", n, err)
	}
}

func TestReadOnlyManagementModesBlockConfigDeployment(t *testing.T) {
	d, g := newTestService(t)
	addSite(t, d, g, "site-a")
	for _, mode := range []string{ManagementModeObserve, ManagementModeLegacy} {
		if err := d.SetManagementMode(g.ID, mode, "alice", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Publish(g.ID, "blocked", "alice", ""); !errors.Is(err, ErrConfigManagementPaused) {
			t.Fatalf("publish in %s mode = %v", mode, err)
		}
		if _, err := d.RequestRelease(g.ID, "blocked", "alice", ""); !errors.Is(err, ErrConfigManagementPaused) {
			t.Fatalf("release request in %s mode = %v", mode, err)
		}
		if _, err := d.StartCanary(g.ID, "blocked", "alice", "", CanaryOptions{}); !errors.Is(err, ErrConfigManagementPaused) {
			t.Fatalf("canary in %s mode = %v", mode, err)
		}
		if got := currentRev(t, d, g); got != "" {
			t.Fatalf("read-only mode changed current revision: %s", got)
		}
		var revisions int64
		if err := d.Store.DB.Table("revisions").Where("group_id = ?", g.ID).Count(&revisions).Error; err != nil {
			t.Fatal(err)
		}
		if revisions != 0 {
			t.Fatalf("read-only mode created revisions: %d", revisions)
		}
	}
	if err := d.SetManagementMode(g.ID, ManagementModeManaged, "alice", ""); err != nil {
		t.Fatal(err)
	}
	dep, err := d.Publish(g.ID, "managed", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if waitDone(t, d, dep).Status != "APPLIED" {
		t.Fatalf("managed publish did not apply")
	}
}

func TestAlignSkipsReadOnlyManagementModes(t *testing.T) {
	d, g := newTestService(t)
	addSite(t, d, g, "site-a")
	dep, err := d.Publish(g.ID, "v1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, d, dep)
	n := &store.Node{GroupID: g.ID, Name: "edge-1", Status: "online", AppliedRevision: ""}
	if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(n); err != nil {
		t.Fatal(err)
	}
	if err := d.SetManagementMode(g.ID, ManagementModeObserve, "alice", ""); err != nil {
		t.Fatal(err)
	}
	d.Align(n.ID)
	var count int64
	if err := d.Store.DB.Table("deployments").Where("group_id = ? AND kind = ?", g.ID, "align").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("observe mode created an automatic alignment: %d", count)
	}
}
