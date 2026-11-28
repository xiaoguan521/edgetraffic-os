package deploy

import (
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestRevisionFilesAndCompareRedactsKeys(t *testing.T) {
	st := storetest.Open(t)
	st.DB.Create(&store.NodeGroup{Name: "default"})

	b1 := render.Repack("r000001", map[string][]byte{
		"conf.d/site-a.conf": []byte("server { listen 80; }\n"),
		"certs/site-a.key":   []byte("SUPER-SECRET-KEY-1"),
	})
	tgz1, err := b1.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	b2 := render.Repack("r000002", map[string][]byte{
		"conf.d/site-a.conf": []byte("server { listen 80; gzip on; }\n"),
		"certs/site-a.key":   []byte("SUPER-SECRET-KEY-2"),
	})
	tgz2, err := b2.TarGz()
	if err != nil {
		t.Fatal(err)
	}

	st.DB.Create(&store.Revision{ID: "r000001", GroupID: 1, Digest: b1.Manifest.Digest, Bundle: tgz1})
	st.DB.Create(&store.Revision{ID: "r000002", GroupID: 1, Digest: b2.Manifest.Digest, Bundle: tgz2})

	svc := &Service{Store: st}
	files, legacy, err := svc.RevisionFiles(1, "r000001")
	if err != nil {
		t.Fatalf("RevisionFiles failed: %v", err)
	}
	if legacy {
		t.Fatalf("expected non-legacy revision")
	}
	for _, f := range files {
		if strings.Contains(f.Content, "SUPER-SECRET-KEY") {
			t.Fatalf("private key leaked in RevisionFiles for %s", f.Path)
		}
		if f.Path == "certs/site-a.key" && !strings.Contains(f.Content, "私钥") {
			t.Fatalf("expected redaction notice for key, got %q", f.Content)
		}
	}

	diffs, err := svc.CompareRevisions(1, "r000001", "r000002")
	if err != nil {
		t.Fatalf("CompareRevisions failed: %v", err)
	}
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(diffs))
	}
	for _, d := range diffs {
		if strings.Contains(d.Diff, "SUPER-SECRET-KEY") {
			t.Fatalf("private key leaked in CompareRevisions for %s", d.Path)
		}
	}
}
