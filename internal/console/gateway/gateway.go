// Package gateway 是控制台的 gRPC AgentGateway 服务端（规格书 5.2、5.3、9.2）。
//
// agent 主动连接到这里；控制台通过已建立的双向流下发签名指令。
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/cmdsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// Server 实现 agentv1.AgentGatewayServer。
type Server struct {
	agentv1.UnimplementedAgentGatewayServer

	Store  *store.Store
	CA     *pki.CA
	CmdKey ed25519.PrivateKey
	Lic    *licsvc.Service
	Audit  *audit.Logger
	Log    *slog.Logger
	// OnOnline 在节点上线（收到 Hello）后调用，用于自动对齐版本。
	OnOnline func(nodeID uint)

	mu       sync.Mutex
	sessions map[uint]*session
}

type session struct {
	nodeID  uint
	out     chan *agentv1.ServerMessage
	mu      sync.Mutex
	waiters map[string]chan *agentv1.CommandResult
	done    chan struct{}
}

// TLSConfig 返回 gRPC 监听使用的 TLS 配置：Enroll 不要求客户端证书，其余 RPC 在处理时校验。
func (s *Server) TLSConfig(hosts []string) (*tls.Config, error) {
	cert, err := s.CA.ServerCert(hosts)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.CA.Cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}, nil
}

// GRPCServer 构造 grpc.Server 并注册服务。
func (s *Server) GRPCServer(tlsCfg *tls.Config) *grpc.Server {
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.MaxRecvMsgSize(4<<20),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	agentv1.RegisterAgentGatewayServer(gs, s)
	return gs
}

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

// NewEnrollToken 生成一次性注册令牌（15 分钟有效），返回明文，库中只存哈希。
func (s *Server) NewEnrollToken(groupID uint, by string) (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := "et_" + hex.EncodeToString(b)
	exp := time.Now().Add(15 * time.Minute)
	err := s.q().EnrollToken.WithContext(bg).Create(&store.EnrollToken{TokenHash: hashToken(tok), GroupID: groupID, ExpiresAt: exp, CreatedBy: by})
	return tok, exp, err
}

// bg 用于不依附请求的写入（例如连接断开后标记离线）。
var bg = context.Background()

func (s *Server) q() *query.Query { return query.Use(s.Store.DB) }

func remoteAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		return p.Addr.String()
	}
	return ""
}

// Enroll 处理节点注册。
func (s *Server) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	ip := remoteAddr(ctx)
	fail := func(code codes.Code, msg string) (*agentv1.EnrollResponse, error) {
		_ = s.Audit.Write(audit.Entry{Username: "agent:" + req.Hostname, IP: ip, Action: "node.enroll", ObjectType: "node", Result: "failure", Detail: map[string]string{"reason": msg}})
		return nil, status.Error(code, msg)
	}
	snap := s.Lic.Snapshot()
	if !snap.Writable() {
		return fail(codes.FailedPrecondition, "license does not allow registering nodes: "+string(snap.State))
	}
	q := s.q()
	et, nd := q.EnrollToken, q.Node
	tok, err := et.WithContext(ctx).Where(et.TokenHash.Eq(hashToken(req.EnrollToken))).First()
	if err != nil {
		return fail(codes.PermissionDenied, "invalid enroll token")
	}
	if tok.UsedAt != nil || time.Now().After(tok.ExpiresAt) {
		return fail(codes.PermissionDenied, "enroll token expired or already used")
	}
	if snap.Payload != nil && snap.Payload.Limits.Nodes > 0 {
		n, err := nd.WithContext(ctx).Where(nd.Status.Neq("revoked")).Count()
		if err != nil {
			return nil, status.Error(codes.Internal, "count nodes")
		}
		if n >= int64(snap.Payload.Limits.Nodes) {
			return fail(codes.ResourceExhausted, fmt.Sprintf("LICENSE_NODE_LIMIT: %d nodes licensed", snap.Payload.Limits.Nodes))
		}
	}
	host := req.Hostname
	if len(host) > 64 {
		host = host[:64]
	}
	node := &store.Node{GroupID: tok.GroupID, Name: host, Hostname: host, RemoteAddr: ip, Status: "offline", AgentVersion: req.AgentVersion}
	var certDER []byte
	if err := nd.WithContext(ctx).Create(node); err != nil {
		return nil, status.Error(codes.Internal, "create node")
	}
	der, serial, err := s.CA.SignNode(req.CsrDer, strconv.FormatUint(uint64(node.ID), 10))
	if err != nil {
		_, _ = nd.WithContext(bg).Delete(node)
		return fail(codes.InvalidArgument, "bad csr: "+err.Error())
	}
	certDER = der
	now := time.Now()
	// 令牌只能用一次：带 used_at IS NULL 条件的更新，并发注册时只有一个能更新成功
	res, err := et.WithContext(ctx).Where(et.ID.Eq(tok.ID), et.UsedAt.IsNull()).Update(et.UsedAt, now)
	if err != nil || res.RowsAffected != 1 {
		_, _ = nd.WithContext(bg).Delete(node)
		return fail(codes.PermissionDenied, "enroll token already used")
	}
	if _, err := nd.WithContext(ctx).Where(nd.ID.Eq(node.ID)).Update(nd.CertSerial, serial.String()); err != nil {
		return nil, status.Error(codes.Internal, "save node certificate serial")
	}
	_ = s.Audit.Write(audit.Entry{Username: "agent:" + host, IP: ip, Action: "node.enroll", ObjectType: "node", ObjectID: strconv.Itoa(int(node.ID)),
		Detail: map[string]any{"group_id": tok.GroupID, "token_created_by": tok.CreatedBy}})
	return &agentv1.EnrollResponse{NodeId: strconv.Itoa(int(node.ID)), ClientCertDer: certDER, CaCertDer: s.CA.Cert.Raw, CommandPubkey: s.CmdKey.Public().(ed25519.PublicKey)}, nil
}

// nodeFromPeer 从已验证的客户端证书识别节点，并核对证书序列号（吊销即失效）。
func (s *Server) nodeFromPeer(ctx context.Context) (*store.Node, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	leaf := ti.State.VerifiedChains[0][0]
	idStr, err := pki.NodeID(leaf)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	id, _ := strconv.Atoi(idStr)
	n, err := store.First[store.Node](s.Store, id)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "unknown node")
	}
	if n.Status == "revoked" || n.CertSerial != leaf.SerialNumber.String() {
		return nil, status.Error(codes.PermissionDenied, "node revoked")
	}
	return n, nil
}

// Connect 处理 agent 的长连接。
func (s *Server) Connect(stream agentv1.AgentGateway_ConnectServer) error {
	ctx := stream.Context()
	node, err := s.nodeFromPeer(ctx)
	if err != nil {
		return err
	}
	sess := &session{nodeID: node.ID, out: make(chan *agentv1.ServerMessage, 16), waiters: map[string]chan *agentv1.CommandResult{}, done: make(chan struct{})}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[uint]*session{}
	}
	if old := s.sessions[node.ID]; old != nil {
		close(old.done)
	}
	s.sessions[node.ID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.sessions[node.ID] == sess {
			delete(s.sessions, node.ID)
			nd := s.q().Node // 连接已断开，流的 ctx 已取消，用 bg
			_, _ = nd.WithContext(bg).Where(nd.ID.Eq(node.ID), nd.Status.Eq("online")).Update(nd.Status, "offline")
			s.Log.Info("node offline", "node", node.ID)
		}
		s.mu.Unlock()
	}()

	errc := make(chan error, 2)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.done:
				errc <- errors.New("superseded by a newer connection")
				return
			case m := <-sess.out:
				if err := stream.Send(m); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			s.onMessage(ctx, node.ID, sess, m)
		}
	}()
	return <-errc
}

func (s *Server) onMessage(ctx context.Context, nodeID uint, sess *session, m *agentv1.AgentMessage) {
	now := time.Now()
	switch b := m.Body.(type) {
	case *agentv1.AgentMessage_Hello:
		ver := strings.SplitN(strings.TrimSpace(b.Hello.SyappVersionOutput), "\n", 2)[0]
		nd := s.q().Node
		_, _ = nd.WithContext(ctx).Where(nd.ID.Eq(nodeID)).UpdateSimple(nd.Status.Value("online"), nd.AgentVersion.Value(b.Hello.AgentVersion),
			nd.SyappVersion.Value(ver), nd.SyappSHA256.Value(b.Hello.SyappSha256), nd.AppliedRevision.Value(b.Hello.AppliedRevisionId),
			nd.LastSeenAt.Value(now), nd.RemoteAddr.Value(remoteAddr(ctx)))
		s.Log.Info("node online", "node", nodeID, "revision", b.Hello.AppliedRevisionId)
		go func() {
			s.PushLicense(nodeID)
			if s.OnOnline != nil {
				s.OnOnline(nodeID)
			}
		}()
	case *agentv1.AgentMessage_Heartbeat:
		hb := b.Heartbeat
		q := s.q()
		nd, rv := q.Node, q.Revision
		drifted := false
		if strings.HasPrefix(hb.TreeHash, "drifted") {
			drifted = true
		} else if n, err := nd.WithContext(ctx).Where(nd.ID.Eq(nodeID)).First(); err == nil && n.AppliedRevision != "" {
			if rev, err := rv.WithContext(ctx).Where(rv.ID.Eq(n.AppliedRevision)).First(); err == nil && hb.TreeHash != "" && hb.TreeHash != rev.Digest {
				drifted = true
			}
		}
		_, _ = nd.WithContext(ctx).Where(nd.ID.Eq(nodeID)).UpdateSimple(nd.Status.Value("online"), nd.CPUPercent.Value(hb.CpuPercent),
			nd.MemUsedBytes.Value(hb.MemUsedBytes), nd.ActiveConns.Value(hb.ActiveConnections), nd.RequestsTotal.Value(hb.RequestsTotal),
			nd.TreeHash.Value(hb.TreeHash), nd.Drifted.Value(drifted), nd.LastSeenAt.Value(now))
		for _, sm := range hb.SiteMinutes {
			_ = q.MetricMinute.WithContext(ctx).Create(&store.MetricMinute{NodeID: nodeID, Site: sm.Site, Minute: sm.MinuteUnix, Requests: sm.Requests,
				S2xx: sm.Status_2Xx, S3xx: sm.Status_3Xx, S4xx: sm.Status_4Xx, S5xx: sm.Status_5Xx, P50ms: sm.P50Ms, P95ms: sm.P95Ms, Bytes: sm.BytesSent})
		}
	case *agentv1.AgentMessage_Result:
		sess.mu.Lock()
		ch := sess.waiters[b.Result.CmdId]
		delete(sess.waiters, b.Result.CmdId)
		sess.mu.Unlock()
		if ch != nil {
			ch <- b.Result
		}
	}
}

// ErrOffline 表示节点当前没有连接。
var ErrOffline = errors.New("node offline")

// Online 报告节点是否在线。
func (s *Server) Online(nodeID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[nodeID] != nil
}

// Send 签名并下发一条指令，等待结果。
func (s *Server) Send(ctx context.Context, nodeID uint, m *agentv1.ServerMessage, timeout time.Duration) (*agentv1.CommandResult, error) {
	s.mu.Lock()
	sess := s.sessions[nodeID]
	s.mu.Unlock()
	if sess == nil {
		return nil, ErrOffline
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	m.CmdId = hex.EncodeToString(id)
	if err := cmdsig.Sign(s.CmdKey, m, time.Now()); err != nil {
		return nil, err
	}
	ch := make(chan *agentv1.CommandResult, 1)
	sess.mu.Lock()
	sess.waiters[m.CmdId] = ch
	sess.mu.Unlock()
	defer func() { sess.mu.Lock(); delete(sess.waiters, m.CmdId); sess.mu.Unlock() }()
	select {
	case sess.out <- m:
	case <-time.After(5 * time.Second):
		return nil, errors.New("send queue full")
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-t.C:
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sess.done:
		return nil, ErrOffline
	}
}

// FetchBundle 向节点提供配置包；节点只能取得本组的版本。
func (s *Server) FetchBundle(req *agentv1.FetchBundleRequest, stream agentv1.AgentGateway_FetchBundleServer) error {
	node, err := s.nodeFromPeer(stream.Context())
	if err != nil {
		return err
	}
	rv := s.q().Revision
	rev, err := rv.WithContext(stream.Context()).Where(rv.ID.Eq(req.RevisionId)).First()
	if err != nil {
		return status.Error(codes.NotFound, "revision not found")
	}
	if rev.GroupID != node.GroupID {
		return status.Error(codes.PermissionDenied, "revision belongs to another node group")
	}
	const chunk = 256 << 10
	for off := 0; off < len(rev.Bundle) || off == 0; off += chunk {
		end := off + chunk
		if end > len(rev.Bundle) {
			end = len(rev.Bundle)
		}
		if err := stream.Send(&agentv1.BundleChunk{Data: rev.Bundle[off:end], Offset: int64(off), Last: end == len(rev.Bundle)}); err != nil {
			return err
		}
		if end == len(rev.Bundle) {
			break
		}
	}
	return nil
}

// PushLicense 向节点推送当前 License。
func (s *Server) PushLicense(nodeID uint) {
	if raw := s.Lic.Raw(); raw != "" {
		_, _ = s.Send(context.Background(), nodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_License{License: &agentv1.LicenseUpdate{Raw: raw}}}, 10*time.Second)
	}
}

// Disconnect 断开节点的连接（吊销时调用）。
func (s *Server) Disconnect(nodeID uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[nodeID]; sess != nil {
		close(sess.done)
		delete(s.sessions, nodeID)
	}
}
