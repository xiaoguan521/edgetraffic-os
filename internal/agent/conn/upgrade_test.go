package conn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

type fakeClient struct {
	agentv1.AgentGatewayClient
	pkg []byte
}

type fakeStream struct {
	grpc.ClientStream
	chunks []*agentv1.BundleChunk
}

func (s *fakeStream) Recv() (*agentv1.BundleChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (f *fakeClient) FetchPackage(ctx context.Context, in *agentv1.FetchPackageRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.BundleChunk], error) {
	return &fakeStream{chunks: []*agentv1.BundleChunk{{Data: f.pkg, Last: true}}}, nil
}

func TestHandleUpgrade(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "wrapper.log")
	wrapper := filepath.Join(dir, "wrapper.sh")
	_ = os.WriteFile(wrapper, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755)
	a := &Agent{C: Config{StateDir: dir, Apply: apply.Config{Wrapper: []string{wrapper}}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.applier = &apply.Applier{C: a.C.Apply}

	pkg := []byte("package")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig, _ := pkgsig.Sign(priv, "etos-ci-pkg-test", "syapp", "1.30.7-1", runtime.GOARCH, "standard", pkg)
	sb, _ := json.Marshal(sig)
	var got []agentv1.CommandResult_Status
	reply := func(st agentv1.CommandResult_Status, msg, _ string) { got = append(got, st) }
	send := func(*agentv1.AgentMessage) {}
	cl := &fakeClient{pkg: pkg}

	a.handleUpgrade(context.Background(), cl, &agentv1.UpgradeDataplane{Action: agentv1.UpgradeDataplane_START, Version: "1.30.7-1", PackageSha256: sig.SHA256, SignatureJson: sb}, reply, send)
	if got[0] != agentv1.CommandResult_APPLIED {
		t.Fatalf("start: %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "upgrade/syapp-1.30.7-1.tar.gz")); string(b) != "package" {
		t.Fatal("package not written to the drop directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "upgrade/syapp-1.30.7-1.tar.gz.sig")); err != nil {
		t.Fatal("signature not written")
	}
	// 控制台给的哈希与签名不符、版本号非法、包被替换：都拒绝且不调用 wrapper
	cl.pkg = []byte("tampered")
	a.handleUpgrade(context.Background(), cl, &agentv1.UpgradeDataplane{Action: agentv1.UpgradeDataplane_START, Version: "1.30.7-1", PackageSha256: sig.SHA256, SignatureJson: sb}, reply, send)
	a.handleUpgrade(context.Background(), cl, &agentv1.UpgradeDataplane{Action: agentv1.UpgradeDataplane_START, Version: "1.30.7-1;id", SignatureJson: sb}, reply, send)
	a.handleUpgrade(context.Background(), cl, &agentv1.UpgradeDataplane{Action: agentv1.UpgradeDataplane_START, Version: "1.30.7-1", PackageSha256: "00", SignatureJson: sb}, reply, send)
	for _, st := range got[1:] {
		if st != agentv1.CommandResult_REJECTED {
			t.Fatalf("expected rejections, got %v", got)
		}
	}
	a.handleUpgrade(context.Background(), cl, &agentv1.UpgradeDataplane{Action: agentv1.UpgradeDataplane_COMMIT}, reply, send)
	calls, _ := os.ReadFile(log)
	if strings.TrimSpace(string(calls)) != "upgrade-start 1.30.7-1\nupgrade-commit" {
		t.Fatalf("wrapper calls:\n%s", calls)
	}
}
