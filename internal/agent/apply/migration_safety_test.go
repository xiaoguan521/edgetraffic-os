package apply

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// An old standalone installation may already use the same local revision ID
// that a newly installed console assigns to its first release.
func TestApplyPreservesCurrentOnRevisionCollision(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "releases", "r000001")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(live, "syedge.conf")
	if err := os.WriteFile(config, []byte("existing standalone configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "r000001"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	bundle, digest := bundleOf(t, render.Input{Revision: "r000001", MimeTypes: []byte("types {}\n"),
		Sites: []render.Site{{Name: "a1", Domains: []string{"a.example.com"}, Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}}}})
	a := &Applier{C: Config{ConfRoot: root, LockFile: filepath.Join(root, "lock"), Wrapper: []string{"false"}}}
	res := a.Apply(context.Background(), "r000001", "r000001", bundle, digest, nil)
	if res.Status != Conflict {
		t.Fatalf("revision collision = %+v, want conflict", res)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "existing standalone configuration" {
		t.Fatalf("current configuration changed: %q, %v", got, err)
	}
}

func TestLegacyMasterMustUseCurrentConfig(t *testing.T) {
	want := "/home/app/node/conf/current/syedge.conf"
	title := brand.MasterTitle() + " /home/app/node/sbin/syedge"
	for _, tc := range []struct {
		args string
		ok   bool
	}{
		{" -p /home/app/node -c " + want, true},
		{" -p/home/app/node -cconf/current/syedge.conf", true},
		{" -p /home/app/node -c /etc/other.conf", false},
		{" -p /home/app/node -c /home/app/node/conf/releases/r000000/syedge.conf", false},
		{" -p /home/app/node", false},
	} {
		if got := legacyMasterUsesCurrent(title+tc.args, "/home/app/node", want); got != tc.ok {
			t.Errorf("%s: got %v", tc.args, got)
		}
	}
	if legacyMasterUsesCurrent("unrelated process -c "+want, "/home/app/node", want) {
		t.Fatal("accepted unrelated process")
	}
}

func TestFailedValidationPreservesExistingRevision(t *testing.T) {
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root, LockFile: filepath.Join(root, "lock"), Wrapper: []string{"false"}}}
	bundle, digest := bundleOf(t, render.Input{Revision: "r000001", MimeTypes: []byte("types {}\n")})
	if err := a.unpack("r000001", bundle, digest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "r000000"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if res := a.Apply(context.Background(), "r000001", "r000000", bundle, digest, nil); res.Status != Invalid {
		t.Fatalf("validation result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "r000001", "syedge.conf")); err != nil {
		t.Fatalf("previously existing revision removed: %v", err)
	}
}

func TestExistingRevisionDoesNotAcceptSymlinks(t *testing.T) {
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root, LockFile: filepath.Join(root, "lock")}}
	bundle, digest := bundleOf(t, render.Input{Revision: "r000001", MimeTypes: []byte("types {}\n")})
	if err := a.unpack("r000001", bundle, digest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "releases", "r000001", "syedge.conf")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.conf")
	if err := os.WriteFile(outside, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if err := a.unpack("r000001", bundle, digest); err == nil {
		t.Fatal("symlink in approved revision was accepted")
	}
}
