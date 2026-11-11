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

// Native bundles are not sandboxed, so a root master may load one only when
// the frozen, approved marker declares it.
func TestLegacyBundleNeedsDeclarationForRootMaster(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "marker.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	orig := masterEUID
	defer func() { masterEUID = orig }()
	uid := 0
	var uidErr error
	masterEUID = func(int) (int, error) { return uid, uidErr }

	for _, tc := range []struct {
		name, marker string
		uid          int
		ok           bool
	}{
		{"root master, undeclared", `{}`, 0, false},
		{"root master, explicitly false", `{"allow_root_master":false}`, 0, false},
		{"root master, declared", `{"checks":[],"allow_root_master":true}`, 0, true},
		{"unprivileged master, undeclared", `{}`, 993, true},
		{"malformed marker never defaults to allowed", `{"allow_root_master":`, 993, false},
		{"wrong type never defaults to allowed", `{"allow_root_master":"yes"}`, 0, false},
	} {
		uid = tc.uid
		r, ok := legacyRootMasterAllowed(write(tc.marker), 1)
		if ok != tc.ok || (!ok && r.Status != Rejected) {
			t.Errorf("%s: ok=%v result=%+v", tc.name, ok, r)
		}
	}
	uid, uidErr = 993, os.ErrPermission
	if r, ok := legacyRootMasterAllowed(write(`{"allow_root_master":true}`), 1); ok || r.Status != Rejected {
		t.Errorf("unknown master user must be refused: ok=%v result=%+v", ok, r)
	}
	uidErr = nil
	if _, ok := legacyRootMasterAllowed(filepath.Join(dir, "missing"), 1); ok {
		t.Error("missing marker accepted")
	}
	// The real reader reports this test process's own effective uid.
	masterEUID = orig
	if got, err := masterEUID(os.Getpid()); err != nil || got != os.Geteuid() {
		t.Errorf("masterEUID(self) = %d, %v; want %d", got, err, os.Geteuid())
	}
}
