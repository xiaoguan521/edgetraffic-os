package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type testBundleStream struct {
	ctx  context.Context
	data []byte
}

func TestLegacySendRequiresCurrentAgentCapability(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sess := &session{nodeID: 1, out: make(chan *agentv1.ServerMessage, 1), waiters: map[string]chan *agentv1.CommandResult{}, done: make(chan struct{})}
	s := &Server{CmdKey: key, sessions: map[uint]*session{1: sess}}
	msg := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Apply{Apply: &agentv1.ApplyRevision{RevisionId: "r000001"}}}
	if _, err := s.SendLegacy(context.Background(), 1, msg, time.Second); !errors.Is(err, ErrLegacyAgentUnsupported) {
		t.Fatalf("old agent accepted native package: %v", err)
	}
	sess.mu.Lock()
	sess.native = true
	sess.mu.Unlock()
	go func() {
		m := <-sess.out
		sess.mu.Lock()
		ch := sess.waiters[m.CmdId]
		sess.mu.Unlock()
		ch <- &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}
	}()
	r, err := s.SendLegacy(context.Background(), 1, msg, time.Second)
	if err != nil || r.Status != agentv1.CommandResult_APPLIED {
		t.Fatalf("capable agent send: %+v, %v", r, err)
	}
}

func (s *testBundleStream) Context() context.Context { return s.ctx }
func (s *testBundleStream) Send(c *agentv1.BundleChunk) error {
	s.data = append(s.data, c.Data...)
	return nil
}
func (s *testBundleStream) SetHeader(metadata.MD) error  { return nil }
func (s *testBundleStream) SendHeader(metadata.MD) error { return nil }
func (s *testBundleStream) SetTrailer(metadata.MD)       {}
func (s *testBundleStream) SendMsg(any) error            { return nil }
func (s *testBundleStream) RecvMsg(any) error            { return nil }

func TestLegacyFetchRequiresApprovalAndDriftHidesContents(t *testing.T) {
	st := storetest.Open(t)
	q := query.Use(st.DB)
	ctx := context.Background()
	group := &store.NodeGroup{Name: "legacy"}
	if err := q.NodeGroup.WithContext(ctx).Create(group); err != nil {
		t.Fatal(err)
	}
	node := &store.Node{Name: "node", GroupID: group.ID, Status: "online", CertSerial: "123"}
	if err := q.Node.WithContext(ctx).Create(node); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := configbundle.SealLegacy(box, []byte("sensitive-config"))
	if err != nil {
		t.Fatal(err)
	}
	rev := &store.Revision{ID: "r000001", GroupID: group.ID, Bundle: sealed}
	if err := q.Revision.WithContext(ctx).Create(rev); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(fmt.Sprintf("%s%d", pki.SpiffePrefix, node.ID))
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{URIs: []*url.URL{u}, SerialNumber: big.NewInt(123)}
	ctx = peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	s := &Server{Store: st, Box: box, Audit: &audit.Logger{DB: st.DB}}
	stream := &testBundleStream{ctx: ctx}
	if err := s.FetchBundle(&agentv1.FetchBundleRequest{RevisionId: rev.ID}, stream); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unapproved bundle fetch = %v", err)
	}
	req := &store.ReleaseRequest{GroupID: group.ID, RevisionID: rev.ID, Status: "approved", RequestedBy: "admin-a", ReviewedBy: "admin-b"}
	if err := q.ReleaseRequest.WithContext(ctx).Create(req); err != nil {
		t.Fatal(err)
	}
	if err := s.FetchBundle(&agentv1.FetchBundleRequest{RevisionId: rev.ID}, stream); err != nil || !bytes.Equal(stream.data, []byte("sensitive-config")) {
		t.Fatalf("approved fetch = %q, %v", stream.data, err)
	}
	s.saveDrift(ctx, node.ID, &agentv1.DriftReport{RevisionId: rev.ID, TreeHash: "hash", Files: []*agentv1.DriftFile{{Path: "lua/script.lua", Content: []byte("secret-data")}}})
	row, err := q.NodeDrift.WithContext(ctx).Where(q.NodeDrift.NodeID.Eq(node.ID)).First()
	if err != nil {
		t.Fatal(err)
	}
	var files []DriftFileView
	if err := json.Unmarshal([]byte(row.Files), &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].Omitted || files[0].Content != "" || bytes.Contains([]byte(row.Files), []byte("secret-data")) {
		t.Fatalf("legacy drift leaked content: %+v", files)
	}
}
