package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

type hook struct {
	mu   sync.Mutex
	reqs []map[string]any
	urls []string
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	h.mu.Lock()
	h.reqs = append(h.reqs, m)
	h.urls = append(h.urls, r.URL.String())
	h.mu.Unlock()
	_, _ = w.Write([]byte(`{"errcode":0}`))
}

func newSvc(t *testing.T, im bool) (*Service, *time.Time) {
	st := storetest.Open(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := &Service{Store: st, Box: box, IMAllow: func() bool { return im }, Now: func() time.Time { return now }}
	// Existing unit tests use httptest servers on loopback. Keep that test-only
	// routing explicit; the production default remains fail-closed.
	s.targetValidator = func(context.Context, *url.URL) error { return nil }
	s.HTTP = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	return s, &now
}

func TestEvaluateFiresAndResolves(t *testing.T) {
	s, now := newSvc(t, true)
	h := &hook{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ch, err := s.SaveChannel(0, ChannelInput{Name: "hook", Type: "webhook", URL: srv.URL, Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	q := query.Use(s.Store.DB)
	ctx := context.Background()
	seen := now.Add(-10 * time.Minute)
	_ = q.Node.WithContext(ctx).Create(&store.Node{Name: "edge-1", Status: "online", LastSeenAt: &seen})
	_ = q.Certificate.WithContext(ctx).Create(&store.Certificate{Name: "shop", NotAfter: now.Add(10 * 24 * time.Hour)})
	for i := 0; i < 3; i++ {
		_ = q.MetricMinute.WithContext(ctx).Create(&store.MetricMinute{Site: "shop", Minute: now.Add(-time.Duration(i) * time.Minute).Unix(), Requests: 100, S5xx: 20})
	}
	for _, k := range []string{KindNodeOffline, KindCertExpiry, KindErrorRate} {
		if err := s.SaveRule(k, true, Defaults[k], []uint{ch.ID}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if len(h.reqs) != 3 {
		t.Fatalf("expected 3 notifications, got %d: %v", len(h.reqs), h.reqs)
	}
	// 再评估一次不重复通知
	if err := s.Evaluate(); err != nil || len(h.reqs) != 3 {
		t.Fatalf("duplicate notifications: %d %v", len(h.reqs), err)
	}
	// 节点恢复心跳 → 发恢复通知
	fresh := *now
	_, _ = q.Node.WithContext(ctx).Where(q.Node.Name.Eq("edge-1")).Update(q.Node.LastSeenAt, fresh)
	if err := s.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if len(h.reqs) != 4 || h.reqs[3]["resolved"] != true {
		t.Fatalf("resolve notification missing: %v", h.reqs)
	}
	evs, _ := q.AlertEvent.WithContext(ctx).Find()
	firing := 0
	for _, e := range evs {
		if e.Status == "firing" {
			firing++
		}
		if !strings.Contains(e.Notify, "已发送") {
			t.Fatalf("notify result %q", e.Notify)
		}
	}
	if firing != 2 {
		t.Fatalf("firing = %d", firing)
	}
}

func TestDingTalkSignAndWeCom(t *testing.T) {
	s, now := newSvc(t, true)
	h := &hook{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	dt, err := s.SaveChannel(0, ChannelInput{Name: "dt", Type: "dingtalk", URL: srv.URL + "/robot/send?access_token=x", Secret: "SECabc", Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dt.SecretEnc, "SECabc") || dt.SecretEnc == "" {
		t.Fatal("secret must be stored encrypted")
	}
	if err := s.Send(dt, Message{Title: "t", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	ts := now.UnixMilli()
	mac := hmac.New(sha256.New, []byte("SECabc"))
	mac.Write([]byte(strings.TrimSpace(strings.Join([]string{itoa(ts), "SECabc"}, "\n"))))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !strings.Contains(h.urls[0], "timestamp="+itoa(ts)) || !strings.Contains(h.urls[0], "sign=") || !strings.Contains(unescape(h.urls[0]), want) {
		t.Fatalf("dingtalk url %s", h.urls[0])
	}
	wc, _ := s.SaveChannel(0, ChannelInput{Name: "wc", Type: "wecom", URL: srv.URL, Enabled: true}, "admin")
	if err := s.Send(wc, Message{Title: "t", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if h.reqs[1]["msgtype"] != "markdown" {
		t.Fatalf("wecom body %v", h.reqs[1])
	}
}

func TestIMRequiresLicense(t *testing.T) {
	s, _ := newSvc(t, false)
	if _, err := s.SaveChannel(0, ChannelInput{Name: "wc", Type: "wecom", URL: "https://qyapi.example.com/x", Enabled: true}, "admin"); !errors.Is(err, ErrIMNotLicensed) {
		t.Fatalf("wecom without alert_im: %v", err)
	}
	if _, err := s.SaveChannel(0, ChannelInput{Name: "h", Type: "webhook", URL: "https://hooks.example.com/x", Enabled: true}, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ChannelInput{
		{Name: "e", Type: "email", URL: "smtp://mail.example.com", MailFrom: "a@b.c", MailTo: "x@y.z"},
		{Name: "e", Type: "email", URL: "smtp://mail.example.com:587", MailFrom: "a@b.c", MailTo: "x@y.z\r\nBcc: evil@x"},
		{Name: "w", Type: "webhook", URL: "file:///etc/passwd"},
		{Name: "w", Type: "webhook", URL: "https://user:pw@x.example.com/"},
	} {
		if _, err := s.SaveChannel(0, bad, "admin"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestRejectsPrivateAlertTargets(t *testing.T) {
	s, _ := newSvc(t, true)
	// Exercise the production validator instead of the local httptest seam.
	s.targetValidator = nil
	s.HTTP = nil
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://10.0.0.1/hook",
		"http://192.168.1.1/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/hook",
		"http://[::1]:8080/hook",
		"http://[fd00::1]/hook",
	} {
		if _, err := s.SaveChannel(0, ChannelInput{Name: "blocked", Type: "webhook", URL: raw, Enabled: true}, "admin"); err == nil {
			t.Errorf("accepted private alert target %q", raw)
		}
	}
	if err := s.Send(&store.AlertChannel{Type: "webhook", URL: "http://169.254.169.254/latest/meta-data/"}, Message{Title: "t", Text: "x"}); err == nil {
		t.Fatal("sent to a metadata endpoint")
	}
}

func TestBlockedAlertAddr(t *testing.T) {
	for _, tc := range []struct {
		ip           string
		public, smtp bool // blocked under policyPublicOnly / policyAllowPrivate
	}{
		{"8.8.8.8", false, false},
		{"2001:4860:4860::8888", false, false},
		{"64:ff9b::808:808", false, false}, // NAT64 of a public address
		{"10.1.2.3", true, false},
		{"172.20.0.1", true, false},
		{"192.168.1.1", true, false},
		{"100.64.0.1", true, false},
		{"198.18.0.1", true, false},
		{"fd12::1", true, false},
		{"fec0::1", true, false},
		{"127.0.0.1", true, true},
		{"0.1.2.3", true, true},
		{"169.254.169.254", true, true},
		{"100.100.100.200", true, true},
		{"fd00:ec2::254", true, true},
		{"240.0.0.1", true, true},
		{"255.255.255.255", true, true},
		{"192.0.0.170", true, true},
		{"::1", true, true},
		{"::", true, true},
		{"::127.0.0.1", true, true},
		{"::ffff:127.0.0.1", true, true},
		{"::ffff:169.254.169.254", true, true},
		{"fe80::1", true, true},
		{"ff02::1", true, true},
		{"64:ff9b::a9fe:a9fe", true, true}, // NAT64 of 169.254.169.254
		{"64:ff9b::a00:1", true, false},    // NAT64 of 10.0.0.1
		{"2002:a9fe:a9fe::1", true, true},  // 6to4 of 169.254.169.254
		{"2002:c0a8:101::1", true, false},  // 6to4 of 192.168.1.1
		{"64:ff9b:1::1", true, true},
		{"2001:0:4136:e378::1", true, true}, // Teredo
	} {
		ip := net.ParseIP(tc.ip)
		if got := blockedAlertIP(ip, policyPublicOnly); got != tc.public {
			t.Errorf("%s public-only blocked=%v, want %v", tc.ip, got, tc.public)
		}
		if got := blockedAlertIP(ip, policyAllowPrivate); got != tc.smtp {
			t.Errorf("%s smtp blocked=%v, want %v", tc.ip, got, tc.smtp)
		}
	}
}

func TestAlertRedirectLimit(t *testing.T) {
	s, _ := newSvc(t, true)
	s.HTTP = nil
	c := s.client()
	req, _ := http.NewRequest(http.MethodPost, "https://hooks.example.com/x", nil)
	if err := c.CheckRedirect(req, make([]*http.Request, maxAlertRedirects-1)); err != nil {
		t.Fatalf("redirect %d rejected: %v", maxAlertRedirects-1, err)
	}
	if err := c.CheckRedirect(req, make([]*http.Request, maxAlertRedirects)); err == nil {
		t.Fatal("unbounded redirects accepted")
	}
	s.targetValidator = nil
	meta, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err := c.CheckRedirect(meta, nil); err == nil {
		t.Fatal("redirect to metadata accepted")
	}
}

func TestSMTPTargetGuard(t *testing.T) {
	s, _ := newSvc(t, true)
	s.targetValidator = nil // production validator
	mail := func(u string) ChannelInput {
		return ChannelInput{Name: "m", Type: "email", URL: u, MailFrom: "a@b.c", MailTo: "x@y.z", Enabled: true}
	}
	for _, raw := range []string{
		"smtp://127.0.0.1:25",
		"smtps://[::1]:465",
		"smtp://169.254.169.254:25",
		"smtp://100.100.100.200:25",
		"smtp://0.0.0.0:25",
		"smtp://[64:ff9b::a9fe:a9fe]:25",
	} {
		if _, err := s.SaveChannel(0, mail(raw), "admin"); err == nil {
			t.Errorf("accepted SMTP target %q", raw)
		}
	}
	// Intranet relays remain allowed for e-mail.
	if _, err := s.SaveChannel(0, mail("smtp://10.0.0.25:587"), "admin"); err != nil {
		t.Fatalf("intranet SMTP relay rejected: %v", err)
	}
	// Send re-checks at dial time, so a stored loopback target never connects.
	err := s.Send(&store.AlertChannel{Type: "email", URL: "smtp://127.0.0.1:25", MailFrom: "a@b.c", MailTo: "x@y.z"}, Message{Title: "t", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "禁止") {
		t.Fatalf("send to loopback SMTP: %v", err)
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

func unescape(u string) string {
	r := strings.NewReplacer("%2B", "+", "%2F", "/", "%3D", "=")
	return r.Replace(u)
}

func TestEvaluateOperationalRules(t *testing.T) {
	s, now := newSvc(t, true)
	diskPct := 92.0
	s.DiskUsage = func(string) (float64, float64, error) {
		return diskPct, 10.0, nil
	}
	for _, k := range []string{KindDeployRollback, KindBackupStale, KindDiskUsage} {
		if err := s.SaveRule(k, true, Defaults[k], nil, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	s.Store.DB.Create(&store.Deployment{
		GroupID:    1,
		RevisionID: "r00001",
		Kind:       "publish",
		Status:     "FAILED",
		StartedAt:  now.Add(-2 * time.Minute),
	})
	if err := s.Evaluate(); err != nil {
		t.Fatal(err)
	}
	var firing []store.AlertEvent
	s.Store.DB.Where("status = ?", "firing").Find(&firing)
	gotKinds := map[string]bool{}
	for _, ev := range firing {
		gotKinds[ev.Kind] = true
	}
	for _, want := range []string{KindDeployRollback, KindBackupStale, KindDiskUsage} {
		if !gotKinds[want] {
			t.Fatalf("expected firing alert for %s, got %+v", want, firing)
		}
	}

	// 记录近期备份并降低磁盘使用率后，backup_stale 与 disk_usage 自动恢复
	_ = s.Store.SetKV("last_backup_at", now.Add(-1*time.Hour).Format(time.RFC3339))
	diskPct = 40.0
	if err := s.Evaluate(); err != nil {
		t.Fatal(err)
	}
	firing = nil
	s.Store.DB.Where("status = ?", "firing").Find(&firing)
	if len(firing) != 1 || firing[0].Kind != KindDeployRollback {
		t.Fatalf("expected only deployment_rolled_back firing after recovery, got %+v", firing)
	}
}
