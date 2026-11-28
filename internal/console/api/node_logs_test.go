package api

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/conn"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

func TestNodeRealtimeLogsEndToEndSSE(t *testing.T) {
	st := storetest.Open(t)
	grp := store.NodeGroup{Name: "log-grp", AllowedCIDRs: "10.0.0.0/8"}
	if err := st.DB.Create(&grp).Error; err != nil {
		t.Fatal(err)
	}
	aud := &audit.Logger{DB: st.DB}
	licPub, licPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lic := &licsvc.Service{Store: st, DataDir: dir, Keys: license.KeyRing{"etos-ci-node-logs": licPub}}
	if err := lic.Init(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rawLic, err := license.Sign(&license.Payload{
		V: 1, KID: "etos-ci-node-logs", LicenseID: "node-logs-test",
		Customer: license.Customer{Name: "test"}, Edition: license.EditionProfessional,
		Features: []license.Feature{license.FeatureCore, license.FeatureAPIToken}, Limits: license.Limits{Nodes: 10, Sites: 50},
		IssuedAt: now.Add(-48 * time.Hour), NotBefore: now.Add(-48 * time.Hour), ExpiresAt: now.Add(365 * 24 * time.Hour),
		Binding: license.Binding{Type: "installation_only", InstallationID: lic.InstallationID()},
	}, licPriv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lic.Import(rawLic, "admin"); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreate(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	_, cmdPriv, _ := ed25519.GenerateKey(rand.Reader)
	box, err := secretbox.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	gw := &gateway.Server{
		Store: st, CA: ca, CmdKey: cmdPriv, Lic: lic, Audit: aud, Log: log, Box: box,
	}
	tlsCfg, err := gw.TLSConfig([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := gw.GRPCServer(tlsCfg)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); _ = lis.Close() })

	srv := &Server{Store: st, GW: gw, Lic: lic, Audit: aud, Log: log}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// 准备 operator 与 viewer 的 API Token
	opTok := "etos_op_logs_token_1234567890"
	vwTok := "etos_vw_logs_token_1234567890"
	st.DB.Create(&store.User{Username: "ops", Role: store.RoleOperator})
	st.DB.Create(&store.User{Username: "viewer", Role: store.RoleViewer})
	st.DB.Create(&store.APIToken{Name: "op", Username: "ops", Role: store.RoleOperator, TokenHash: hashToken(opTok), Prefix: opTok[:12]})
	st.DB.Create(&store.APIToken{Name: "vw", Username: "viewer", Role: store.RoleViewer, TokenHash: hashToken(vwTok), Prefix: vwTok[:12]})

	// 启动真实 Agent 并注册连接
	agentDir := t.TempDir()
	logDir := filepath.Join(agentDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	accessPath := filepath.Join(logDir, "access.json")
	errorPath := filepath.Join(logDir, "error.log")
	initAccess := `{"ts":"2026-11-28T21:00:00+08:00","site":"shop.example.com","status":200,"method":"GET","uri":"/init","rt":0.012,"ip":"10.0.0.1"}` + "\n"
	initError := "2026/11/28 21:00:00 [error] 100#0: *1 upstream timed out\n"
	if err := os.WriteFile(accessPath, []byte(initAccess), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorPath, []byte(initError), 0o644); err != nil {
		t.Fatal(err)
	}

	tok, _, err := gw.NewEnrollToken(grp.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	caSum := sha256.Sum256(ca.Cert.Raw)
	ag := conn.New(conn.Config{
		Server:    lis.Addr().String(),
		StateDir:  filepath.Join(agentDir, "agent"),
		AccessLog: accessPath,
		Heartbeat: 200 * time.Millisecond,
		Apply:     apply.Config{ConfRoot: filepath.Join(agentDir, "conf"), ErrorLog: errorPath},
	}, log)
	if err := ag.Enroll(context.Background(), tok, hex.EncodeToString(caSum[:])); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	agCtx, agCancel := context.WithCancel(context.Background())
	t.Cleanup(agCancel)
	go func() { _ = ag.Run(agCtx) }()

	var node store.Node
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := st.DB.Order("id desc").First(&node).Error; err == nil && gw.Online(node.ID) && node.DiskUsedPercent > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gw.Online(node.ID) {
		t.Fatal("agent did not come online")
	}
	if node.DiskUsedPercent <= 0 {
		t.Fatalf("expected heartbeat to report disk usage > 0, got %v", node.DiskUsedPercent)
	}

	// 1. viewer 角色禁止查看实时日志
	reqVw, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/nodes/%d/logs/stream?file=access", ts.URL, node.ID), nil)
	reqVw.Header.Set("Authorization", "Bearer "+vwTok)
	respVw, err := http.DefaultClient.Do(reqVw)
	if err != nil {
		t.Fatal(err)
	}
	_ = respVw.Body.Close()
	if respVw.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", respVw.StatusCode)
	}

	// 2. 非法 file 参数（防止任意文件读取）被拒绝
	reqBad, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/nodes/%d/logs/stream?file=/etc/passwd", ts.URL, node.ID), nil)
	reqBad.Header.Set("Authorization", "Bearer "+opTok)
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatal(err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid file status = %d, want 400", respBad.StatusCode)
	}

	// 3. operator 拉取实时 access.json：先收到初始行，再实时收到新追加行
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer streamCancel()
	reqSSE, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, fmt.Sprintf("%s/api/v1/nodes/%d/logs/stream?file=access&seconds=30", ts.URL, node.ID), nil)
	reqSSE.Header.Set("Authorization", "Bearer "+opTok)
	respSSE, err := http.DefaultClient.Do(reqSSE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = respSSE.Body.Close() }()
	if respSSE.StatusCode != http.StatusOK || !strings.HasPrefix(respSSE.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse response = %d ct=%q", respSSE.StatusCode, respSSE.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(respSSE.Body)
	readChunkContaining := func(want string) {
		t.Helper()
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				t.Fatalf("SSE read failed before %q: %v", want, rerr)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var payload struct {
				File  string   `json:"file"`
				Lines []string `json:"lines"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload) != nil {
				continue
			}
			for _, l := range payload.Lines {
				if strings.Contains(l, want) {
					return
				}
			}
		}
	}
	readChunkContaining(`"/init"`)

	// 追加新日志并确认实时推送
	f, err := os.OpenFile(accessPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"ts":"2026-11-28T21:00:05+08:00","site":"shop.example.com","status":502,"method":"POST","uri":"/live-502","rt":1.25,"ip":"10.0.0.2"}` + "\n")
	_ = f.Close()
	readChunkContaining(`"/live-502"`)
	streamCancel()
	_ = respSSE.Body.Close()

	// 4. 拉取 error.log 同样工作
	errCtx, errCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer errCancel()
	reqErr, _ := http.NewRequestWithContext(errCtx, http.MethodGet, fmt.Sprintf("%s/api/v1/nodes/%d/logs/stream?file=error&seconds=15", ts.URL, node.ID), nil)
	reqErr.Header.Set("Authorization", "Bearer "+opTok)
	respErr, err := http.DefaultClient.Do(reqErr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = respErr.Body.Close() }()
	errReader := bufio.NewReader(respErr.Body)
	for {
		line, rerr := errReader.ReadString('\n')
		if rerr != nil {
			t.Fatalf("error.log SSE failed: %v", rerr)
		}
		if strings.Contains(line, "upstream timed out") {
			break
		}
	}
	errCancel()
	_ = respErr.Body.Close()

	// 5. 审计记录已写入 node.tail_log
	var auditCount int64
	st.DB.Model(&store.AuditLog{}).Where("action = ?", "node.tail_log").Count(&auditCount)
	if auditCount < 2 {
		t.Fatalf("expected >= 2 node.tail_log audit records, got %d", auditCount)
	}

	// 6. 离线节点返回 409 NODE_OFFLINE
	offlineNode := store.Node{GroupID: grp.ID, Name: "offline-node", Status: "offline"}
	st.DB.Create(&offlineNode)
	reqOff, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/nodes/%d/logs/stream?file=access", ts.URL, offlineNode.ID), nil)
	reqOff.Header.Set("Authorization", "Bearer "+opTok)
	respOff, err := http.DefaultClient.Do(reqOff)
	if err != nil {
		t.Fatal(err)
	}
	_ = respOff.Body.Close()
	if respOff.StatusCode != http.StatusConflict {
		t.Fatalf("offline node status = %d, want 409", respOff.StatusCode)
	}
}
