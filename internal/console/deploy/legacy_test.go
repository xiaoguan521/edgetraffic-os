package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func legacyArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestLegacyRequestNeedsSecondAdministratorAndEncryptsBundle(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "admin-a", ""); err != nil {
		t.Fatal(err)
	}
	n := &store.Node{GroupID: g.ID, Name: "edge", Status: "online", AppliedRevision: "r000000"}
	if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(n); err != nil {
		t.Fatal(err)
	}
	d.Agents = &applyAgents{d: d}
	data := legacyArchive(t, map[string]string{
		"syedge.conf":  `events {} http { init_by_lua_file @SYEDGE_RELEASE@/lua/init.lua; }`,
		"lua/init.lua": `local secret = "do-not-expose"`,
	})
	req, err := d.RequestLegacy(g.ID, "migration", data, nil, LegacyOptions{}, "admin-a", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := d.revision(req.RevisionID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rev.Bundle, []byte("do-not-expose")) || !isLegacyRevision(rev) {
		t.Fatal("legacy bundle was not encrypted or marked")
	}
	files, err := d.revisionFiles(rev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(files["syedge.conf"]), legacyReleaseToken) || !strings.Contains(string(files["syedge.conf"]), req.RevisionID+"/lua/init.lua") {
		t.Fatalf("immutable release path not resolved: %s", files["syedge.conf"])
	}
	if _, err := d.Approve(req.ID, "", "releaser", ""); !errors.Is(err, ErrLegacyAdminRequired) {
		t.Fatalf("ordinary release approval accepted native configuration: %v", err)
	}
	if _, err := d.ApproveLegacy(req.ID, "", "admin-a", ""); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval = %v", err)
	}
	detail, err := d.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range detail.Files {
		if strings.Contains(f.Diff, "do-not-expose") {
			t.Fatal("legacy secret leaked in approval preview")
		}
	}
	dep, err := d.ApproveLegacy(req.ID, "approved", "admin-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, d, dep); got.Status != "APPLIED" {
		t.Fatalf("legacy deployment = %+v", got)
	}
	if got := currentRev(t, d, g); got != req.RevisionID {
		t.Fatalf("current revision = %s", got)
	}
}

type legacyRecordingAgents struct {
	applies   atomic.Int32
	rollbacks atomic.Int32
}

func (a *legacyRecordingAgents) Online(uint) bool { return true }
func (a *legacyRecordingAgents) SendLegacy(ctx context.Context, id uint, m *agentv1.ServerMessage, timeout time.Duration) (*agentv1.CommandResult, error) {
	return a.Send(ctx, id, m, timeout)
}
func (a *legacyRecordingAgents) Send(_ context.Context, _ uint, m *agentv1.ServerMessage, _ time.Duration) (*agentv1.CommandResult, error) {
	if m.GetApply() != nil {
		a.applies.Add(1)
	}
	if m.GetRollback() != nil {
		a.rollbacks.Add(1)
	}
	return &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}, nil
}

func TestLegacyRollbackRequiresNewApprovalAndVerifiesDigest(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(&store.Node{GroupID: g.ID, Name: "edge", Status: "online", AppliedRevision: "r000000"}); err != nil {
		t.Fatal(err)
	}
	ag := &legacyRecordingAgents{}
	d.Agents = ag
	var revisions []string
	for _, value := range []string{"one", "two"} {
		req, err := d.RequestLegacy(g.ID, value, legacyArchive(t, map[string]string{"syedge.conf": value}), nil, LegacyOptions{}, "a", "")
		if err != nil {
			t.Fatal(err)
		}
		dep, err := d.ApproveLegacy(req.ID, "", "b", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := waitDone(t, d, dep); got.Status != "APPLIED" {
			t.Fatalf("apply: %+v", got)
		}
		revisions = append(revisions, req.RevisionID)
	}
	req, err := d.RequestLegacyRollback(g.ID, revisions[0], "rollback", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApproveLegacy(req.ID, "", "a", ""); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	dep, err := d.ApproveLegacy(req.ID, "", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, d, dep); got.Status != "APPLIED" || got.Kind != "rollback" {
		t.Fatalf("rollback: %+v", got)
	}
	if ag.rollbacks.Load() != 0 || ag.applies.Load() != 3 {
		t.Fatalf("rollback did not verify its frozen bundle: applies=%d rollbacks=%d", ag.applies.Load(), ag.rollbacks.Load())
	}
}

func TestLegacyArchiveRejectsUnsafeOrDuplicatePaths(t *testing.T) {
	for _, files := range []map[string]string{
		{"../escape": "bad", "syedge.conf": "events {}"},
		{"/absolute": "bad", "syedge.conf": "events {}"},
		{"MANIFEST.json": "bad", "syedge.conf": "events {}"},
		{"lua/a.lua": "missing main"},
	} {
		if _, err := readLegacyArchive(legacyArchive(t, files)); err == nil {
			t.Fatalf("unsafe archive accepted: %v", files)
		}
	}
}

func TestLegacyRootMasterDeclarationIsFrozenAndVisible(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "admin-a", ""); err != nil {
		t.Fatal(err)
	}
	policyOf := func(opts LegacyOptions) (configbundle.LegacyPolicy, *store.ReleaseRequest) {
		t.Helper()
		req, err := d.RequestLegacy(g.ID, "migration", legacyArchive(t, map[string]string{"syedge.conf": "events {}"}), nil, opts, "admin-a", "")
		if err != nil {
			t.Fatal(err)
		}
		rev, err := d.revision(req.RevisionID, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		files, err := d.revisionFiles(rev)
		if err != nil {
			t.Fatal(err)
		}
		p, err := configbundle.ParseLegacyPolicy(files[configbundle.LegacyMarker])
		if err != nil {
			t.Fatal(err)
		}
		if err := d.close(d.q(), req.ID, ApprovalRejected, "admin-b", "", 0); err != nil {
			t.Fatal(err)
		}
		return p, req
	}
	if p, req := policyOf(LegacyOptions{}); p.AllowRootMaster || strings.Contains(req.Note, legacyRootMasterNote) {
		t.Fatalf("root masters allowed without a declaration: %+v, note %q", p, req.Note)
	}
	if p, req := policyOf(LegacyOptions{AllowRootMaster: true}); !p.AllowRootMaster || !strings.Contains(req.Note, legacyRootMasterNote) {
		t.Fatalf("declaration not frozen into the bundle or not shown to the approver: %+v, note %q", p, req.Note)
	}
}

func TestLegacyReviewDownloadHidesPrivateKeys(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "admin-a", ""); err != nil {
		t.Fatal(err)
	}
	pem := "-----BEGIN EC PRIVATE KEY-----\nKEYMATERIAL-ONE\n-----END EC PRIVATE KEY-----\n"
	req, err := d.RequestLegacy(g.ID, "migration", legacyArchive(t, map[string]string{
		"syedge.conf":    "events {}",
		"lua/init.lua":   "local reviewed = true",
		"certs/site.key": "KEYMATERIAL-TWO",
		"certs/site.pem": pem,
		"certs/site.crt": "-----BEGIN CERTIFICATE-----\nPUBLIC\n-----END CERTIFICATE-----\n",
	}), nil, LegacyOptions{}, "admin-a", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := d.LegacyRequestBundle(req.ID, "admin-b", "")
	if err != nil {
		t.Fatal(err)
	}
	files, err := render.ReadTarGz(data)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if bytes.Contains(body, []byte("KEYMATERIAL")) {
			t.Errorf("%s: private key content in the review download", name)
		}
	}
	for _, name := range []string{"certs/site.key", "certs/site.pem"} {
		if !bytes.Contains(files[name], []byte("SHA-256: ")) {
			t.Errorf("%s: no hash placeholder: %q", name, files[name])
		}
	}
	if string(files["lua/init.lua"]) != "local reviewed = true" || !bytes.Contains(files["certs/site.crt"], []byte("PUBLIC")) {
		t.Errorf("reviewable files were altered: %q", files)
	}
	// The stored, approved bundle itself is untouched.
	rev, err := d.revision(req.RevisionID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := d.revisionFiles(rev)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored["certs/site.key"]) != "KEYMATERIAL-TWO" || string(stored["certs/site.pem"]) != pem {
		t.Fatal("redaction changed the frozen bundle")
	}
}
