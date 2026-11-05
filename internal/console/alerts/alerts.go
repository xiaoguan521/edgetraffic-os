// Package alerts 是控制台的告警（规格书 7.1 M9）：内置规则（节点离线、5xx 率、证书到期、配置漂移）
// 定时评估，状态变化（触发 / 恢复）时推送到邮件、Webhook、企业微信或钉钉机器人。
//
// 企业微信与钉钉需要授权功能 alert_im；通道密钥（钉钉加签密钥、SMTP 口令）加密存储，接口不返回。
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

// 规则种类。
const (
	KindNodeOffline    = "node_offline"           // 阈值：离线分钟数
	KindErrorRate      = "error_rate"             // 阈值：近 5 分钟 5xx 占比（%），请求数不少于 MinRequests 才计算
	KindCertExpiry     = "cert_expiry"            // 阈值：剩余天数
	KindDrift          = "drift"                  // 节点配置被手工改动
	KindDeployRollback = "deployment_rolled_back" // 近期发布失败或灰度自动回滚
	KindBackupStale    = "backup_stale"           // 阈值：距上次成功备份的小时数
	KindDiskUsage      = "disk_usage"             // 阈值：控制台数据目录磁盘或 inode 使用率（%）
)

// Defaults 是规则的默认阈值。
var Defaults = map[string]float64{
	KindNodeOffline:    3,
	KindErrorRate:      5,
	KindCertExpiry:     30,
	KindDrift:          0,
	KindDeployRollback: 0,
	KindBackupStale:    24,
	KindDiskUsage:      85,
}

// KindName 是规则的中文名。
var KindName = map[string]string{
	KindNodeOffline:    "节点离线",
	KindErrorRate:      "5xx 错误率",
	KindCertExpiry:     "证书即将到期",
	KindDrift:          "配置被手工改动",
	KindDeployRollback: "发布失败或自动回滚",
	KindBackupStale:    "备份超期未执行",
	KindDiskUsage:      "控制台磁盘空间不足",
}

// MinRequests 是计算 5xx 率的最少请求数（流量太小时比例没有意义）。
const MinRequests = 50

// Service 是告警服务。
type Service struct {
	Store     *store.Store
	Box       *secretbox.Box
	Log       *slog.Logger
	IMAllow   func() bool // 授权是否包含 alert_im
	HTTP      *http.Client
	Now       func() time.Time
	DataDir   string
	DiskUsage func(dir string) (usedPct, inodePct float64, err error)

	// targetValidator is kept private so production callers cannot accidentally
	// disable the webhook target guard. Tests use it to route requests to a
	// local httptest server.
	targetValidator func(context.Context, *url.URL) error
}

func (s *Service) q() *query.Query { return query.Use(s.Store.DB) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := lookupAlertIPs(ctx, host, policyPublicOnly)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("告警地址没有可连接的 IP")
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		// A custom CheckRedirect replaces net/http's default 10-hop limit, so
		// enforce it here as well.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxAlertRedirects {
				return errors.New("告警地址重定向次数过多")
			}
			return s.validateAlertTarget(req.Context(), req.URL)
		},
	}
}

var bg = context.Background()

const maxAlertRedirects = 10

// ipPolicy selects which address ranges an alert channel may reach.
type ipPolicy int

const (
	// policyPublicOnly is used for webhook / WeCom / DingTalk targets.
	policyPublicOnly ipPolicy = iota
	// policyAllowPrivate is used for SMTP: mail relays commonly sit on the
	// intranet, but loopback, link-local and cloud metadata stay blocked.
	policyAllowPrivate
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

var (
	// Never reachable from any alert channel.
	alwaysBlockedPrefixes = mustPrefixes(
		"0.0.0.0/8",          // "this network", including 0.0.0.0
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local, including 169.254.169.254 metadata
		"192.0.0.0/24",       // IETF protocol assignments
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved, including 255.255.255.255
		"100.100.100.200/32", // Alibaba Cloud metadata (inside 100.64.0.0/10)
		"::/96",              // unspecified and deprecated IPv4-compatible addresses
		"::1/128",            // loopback
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
		"64:ff9b:1::/48",     // local-use NAT64 (RFC 8215), embedding is not fixed
		"2001::/32",          // Teredo, can tunnel to arbitrary IPv4
		"fd00:ec2::254/128",  // AWS IPv6 metadata
	)
	// Blocked for policyPublicOnly in addition to alwaysBlockedPrefixes.
	privateBlockedPrefixes = mustPrefixes(
		"10.0.0.0/8",
		"100.64.0.0/10", // RFC 6598 shared address space
		"172.16.0.0/12",
		"192.168.0.0/16",
		"198.18.0.0/15",                                     // benchmarking
		"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", // documentation
		"fc00::/7",      // unique local
		"fec0::/10",     // deprecated site-local
		"2001:db8::/32", // documentation
	)
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052, IPv4 in the last 32 bits
	sixToFour      = netip.MustParsePrefix("2002::/16")    // RFC 3056, IPv4 in bits 16-47
)

func blockedAlertIP(ip net.IP, policy ipPolicy) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return blockedAlertAddr(addr, policy)
}

func blockedAlertAddr(a netip.Addr, policy ipPolicy) bool {
	a = a.Unmap()
	if a.Is6() {
		// IPv6 forms that embed an IPv4 address are judged by that address.
		b := a.As16()
		if nat64WellKnown.Contains(a) {
			return blockedAlertAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), policy)
		}
		if sixToFour.Contains(a) {
			return blockedAlertAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), policy)
		}
	}
	for _, p := range alwaysBlockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if policy == policyPublicOnly {
		for _, p := range privateBlockedPrefixes {
			if p.Contains(a) {
				return true
			}
		}
	}
	return false
}

func lookupAlertIPs(ctx context.Context, host string, policy ipPolicy) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("告警地址主机解析失败: %w", err)
	}
	if len(ips) == 0 {
		return nil, errors.New("告警地址没有解析到 IP")
	}
	for _, ip := range ips {
		if blockedAlertIP(ip, policy) {
			if policy == policyAllowPrivate {
				return nil, fmt.Errorf("邮件服务器解析到禁止访问的本机、链路本地或云元数据地址 %q", ip.String())
			}
			return nil, fmt.Errorf("告警地址解析到禁止访问的本机、内网或链路本地地址 %q", ip.String())
		}
	}
	return ips, nil
}

func validateAlertURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return errors.New("地址不能为空")
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" || u.User != nil || u.Hostname() == "" {
			return errors.New("地址必须是 http(s) URL，且不得包含用户信息")
		}
		_, err := lookupAlertIPs(ctx, u.Hostname(), policyPublicOnly)
		return err
	case "smtp", "smtps":
		if u.Hostname() == "" || u.Port() == "" {
			return errors.New("邮件服务器格式为 smtp://主机:端口 或 smtps://主机:端口")
		}
		_, err := lookupAlertIPs(ctx, u.Hostname(), policyAllowPrivate)
		return err
	}
	return errors.New("地址必须是 http(s) URL，且不得包含用户信息")
}

func (s *Service) validateAlertTarget(ctx context.Context, u *url.URL) error {
	if s.targetValidator != nil {
		return s.targetValidator(ctx, u)
	}
	return validateAlertURL(ctx, u)
}

func (s *Service) validateChannelTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return s.validateAlertTarget(ctx, u)
}

// Rules 返回全部规则（不存在的按默认值补齐，默认关闭）。
func (s *Service) Rules() ([]*store.AlertRule, error) {
	rows, err := s.q().AlertRule.WithContext(bg).Find()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Kind] = true
	}
	for k, v := range Defaults {
		if !have[k] {
			rows = append(rows, &store.AlertRule{Kind: k, Threshold: v})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Kind < rows[j].Kind })
	return rows, nil
}

// SaveRule 保存一条规则。
func (s *Service) SaveRule(kind string, enabled bool, threshold float64, channels []uint, user string) error {
	if _, ok := Defaults[kind]; !ok {
		return fmt.Errorf("未知规则 %q", kind)
	}
	if threshold < 0 || threshold > 100000 {
		return errors.New("阈值超出范围")
	}
	var ids []string
	for _, c := range channels {
		ids = append(ids, strconv.FormatUint(uint64(c), 10))
	}
	ar := s.q().AlertRule
	row, err := ar.WithContext(bg).Where(ar.Kind.Eq(kind)).First()
	if err != nil {
		row = &store.AlertRule{Kind: kind}
	}
	row.Enabled, row.Threshold, row.ChannelIDs, row.UpdatedBy = enabled, threshold, strings.Join(ids, ","), user
	return ar.WithContext(bg).Save(row)
}

// ChannelInput 是通道配置（Secret 为空表示保持原值）。
type ChannelInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Secret   string `json:"secret"`
	SMTPUser string `json:"smtp_user"`
	MailFrom string `json:"mail_from"`
	MailTo   string `json:"mail_to"`
	Enabled  bool   `json:"enabled"`
}

// ErrIMNotLicensed 表示授权不包含企业微信 / 钉钉告警。
var ErrIMNotLicensed = errors.New("当前授权不包含功能 alert_im（企业微信 / 钉钉告警）")

func (in *ChannelInput) validate(imAllowed bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		return errors.New("通道名称必填，最长 64 个字符")
	}
	switch in.Type {
	case "webhook", "wecom", "dingtalk":
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return errors.New("地址必须是 http(s) URL")
		}
		if in.Type != "webhook" && !imAllowed {
			return ErrIMNotLicensed
		}
	case "email":
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" || u.Port() == "" {
			return errors.New("邮件服务器格式为 smtp://主机:端口（STARTTLS）或 smtps://主机:端口")
		}
		if !strings.Contains(in.MailFrom, "@") || in.MailTo == "" {
			return errors.New("请填写发件人与收件人")
		}
		for _, a := range strings.Split(in.MailTo, ",") {
			if !strings.Contains(a, "@") || strings.ContainsAny(a, "\r\n") {
				return fmt.Errorf("收件人地址 %q 无效", a)
			}
		}
		if strings.ContainsAny(in.MailFrom, "\r\n") {
			return errors.New("发件人地址无效")
		}
	default:
		return fmt.Errorf("未知通道类型 %q", in.Type)
	}
	return nil
}

// SaveChannel 新建（id 为 0）或修改通道。
func (s *Service) SaveChannel(id uint, in ChannelInput, user string) (*store.AlertChannel, error) {
	if err := in.validate(s.IMAllow == nil || s.IMAllow()); err != nil {
		return nil, err
	}
	if in.Type == "webhook" || in.Type == "wecom" || in.Type == "dingtalk" || in.Type == "email" {
		if err := s.validateChannelTarget(in.URL); err != nil {
			return nil, err
		}
	}
	ac := s.q().AlertChannel
	row := &store.AlertChannel{CreatedBy: user}
	if id != 0 {
		r, err := ac.WithContext(bg).Where(ac.ID.Eq(id)).First()
		if err != nil {
			return nil, store.ErrNotFound
		}
		row = r
	}
	row.Name, row.Type, row.URL, row.SMTPUser, row.MailFrom, row.MailTo, row.Enabled = in.Name, in.Type, in.URL, in.SMTPUser, in.MailFrom, in.MailTo, in.Enabled
	if in.Secret != "" {
		enc, err := s.Box.Seal(in.Secret)
		if err != nil {
			return nil, err
		}
		row.SecretEnc = enc
	}
	if err := ac.WithContext(bg).Save(row); err != nil {
		return nil, err
	}
	return row, nil
}

// Message 是一条待发送的告警。
type Message struct {
	Title    string
	Text     string
	Resolved bool
}

// Send 通过一个通道发送消息。
func (s *Service) Send(ch *store.AlertChannel, m Message) error {
	secret := ""
	if ch.SecretEnc != "" {
		v, err := s.Box.Open(ch.SecretEnc)
		if err != nil {
			return err
		}
		secret = v
	}
	if (ch.Type == "wecom" || ch.Type == "dingtalk") && s.IMAllow != nil && !s.IMAllow() {
		return ErrIMNotLicensed
	}
	md := fmt.Sprintf("### %s\n%s", m.Title, m.Text)
	switch ch.Type {
	case "webhook":
		return s.post(ch.URL, map[string]any{"title": m.Title, "text": m.Text, "resolved": m.Resolved, "source": "EdgeTraffic OS"})
	case "wecom":
		return s.post(ch.URL, map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": md}})
	case "dingtalk":
		u := ch.URL
		if secret != "" {
			ts := strconv.FormatInt(s.now().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(ts + "\n" + secret))
			sep := "&"
			if !strings.Contains(u, "?") {
				sep = "?"
			}
			u += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		}
		return s.post(u, map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": m.Title, "text": md}})
	case "email":
		return s.mail(ch, secret, m)
	}
	return fmt.Errorf("unknown channel type %q", ch.Type)
}

func (s *Service) post(u string, body any) error {
	b, _ := json.Marshal(body)
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if err := s.validateAlertTarget(ctx, parsed); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 企业微信 / 钉钉在 HTTP 200 中用 errcode 表示失败
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("errcode %d: %s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

// dialSMTP resolves the mail server, rejects loopback / link-local / metadata
// addresses, then connects to the checked IP itself so a DNS rebinding answer
// cannot swap the target between the check and the connection.
func (s *Service) dialSMTP(u *url.URL) (net.Conn, error) {
	host, port := u.Hostname(), u.Port()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	var addrs []string
	if s.targetValidator != nil {
		// Test seam: route to a local server without the production guard.
		if err := s.targetValidator(ctx, u); err != nil {
			return nil, err
		}
		addrs = []string{net.JoinHostPort(host, port)}
	} else {
		ips, err := lookupAlertIPs(ctx, host, policyAllowPrivate)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			addrs = append(addrs, net.JoinHostPort(ip.String(), port))
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, addr := range addrs {
		var conn net.Conn
		var err error
		if u.Scheme == "smtps" {
			// ServerName stays the configured host name for certificate checks.
			conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		} else {
			conn, err = d.DialContext(ctx, "tcp", addr)
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("邮件服务器没有可连接的 IP")
	}
	return nil, lastErr
}

func (s *Service) mail(ch *store.AlertChannel, pass string, m Message) error {
	u, err := url.Parse(ch.URL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	to := strings.Split(ch.MailTo, ",")
	for i := range to {
		to[i] = strings.TrimSpace(to[i])
	}
	subject := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(m.Title)) + "?="
	msg := "From: " + ch.MailFrom + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(m.Text)) + "\r\n"
	conn, err := s.dialSMTP(u)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if u.Scheme == "smtp" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("邮件服务器不支持 STARTTLS，拒绝明文发送")
		}
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if ch.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", ch.SMTPUser, pass, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(ch.MailFrom); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(a); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

type finding struct {
	kind, key, msg string
}

// Evaluate 评估一次全部启用的规则，对新触发与已恢复的告警发送通知。
func (s *Service) Evaluate() error {
	rules, err := s.Rules()
	if err != nil {
		return err
	}
	q := s.q()
	now := s.now()
	var found []finding
	enabled := map[string]*store.AlertRule{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		enabled[r.Kind] = r
		switch r.Kind {
		case KindNodeOffline:
			nodes, err := q.Node.WithContext(bg).Where(q.Node.Status.Neq("revoked"), q.Node.LastSeenAt.IsNotNull()).Find()
			if err != nil {
				return err
			}
			for _, n := range nodes {
				if now.Sub(*n.LastSeenAt) >= time.Duration(r.Threshold*float64(time.Minute)) {
					found = append(found, finding{r.Kind, "node:" + n.Name, fmt.Sprintf("节点 %s 已离线（最后心跳 %s）", n.Name, n.LastSeenAt.Format("01-02 15:04:05"))})
				}
			}
		case KindDrift:
			nodes, err := q.Node.WithContext(bg).Where(q.Node.Drifted.Is(true), q.Node.Status.Neq("revoked")).Find()
			if err != nil {
				return err
			}
			for _, n := range nodes {
				found = append(found, finding{r.Kind, "node:" + n.Name, fmt.Sprintf("节点 %s 的配置被手工改动，请在控制台恢复或采纳", n.Name)})
			}
		case KindCertExpiry:
			certs, err := q.Certificate.WithContext(bg).Where(q.Certificate.NotAfter.Lt(now.Add(time.Duration(r.Threshold * 24 * float64(time.Hour))))).Find()
			if err != nil {
				return err
			}
			for _, c := range certs {
				days := int(c.NotAfter.Sub(now).Hours() / 24)
				found = append(found, finding{r.Kind, "cert:" + c.Name, fmt.Sprintf("证书 %s 将于 %s 到期（剩余 %d 天）", c.Name, c.NotAfter.Format("2006-01-02"), days)})
			}
		case KindErrorRate:
			mm := q.MetricMinute
			rows, err := mm.WithContext(bg).Where(mm.Minute.Gte(now.Add(-5 * time.Minute).Unix())).Find()
			if err != nil {
				return err
			}
			type agg struct{ req, s5 uint64 }
			by := map[string]*agg{}
			for _, m := range rows {
				a := by[m.Site]
				if a == nil {
					a = &agg{}
					by[m.Site] = a
				}
				a.req += m.Requests
				a.s5 += m.S5xx
			}
			for site, a := range by {
				if a.req >= MinRequests {
					if pct := float64(a.s5) * 100 / float64(a.req); pct >= r.Threshold {
						found = append(found, finding{r.Kind, "site:" + site, fmt.Sprintf("站点 %s 近 5 分钟 5xx 占比 %.1f%%（%d / %d 请求）", site, pct, a.s5, a.req)})
					}
				}
			}
		case KindDeployRollback:
			var deps []store.Deployment
			if err := s.Store.DB.Where("started_at >= ? AND status IN ?", now.Add(-10*time.Minute), []string{"FAILED", "ROLLED_BACK", "PARTIAL"}).Find(&deps).Error; err != nil {
				return err
			}
			for _, d := range deps {
				found = append(found, finding{r.Kind, fmt.Sprintf("deployment:%d", d.ID), fmt.Sprintf("节点组 #%d 发布版本 %s 异常（状态 %s）", d.GroupID, d.RevisionID, d.Status)})
			}
			var canaries []store.CanaryRelease
			if err := s.Store.DB.Where("created_at >= ? AND status IN ?", now.Add(-10*time.Minute), []string{"rolled_back", "failed"}).Find(&canaries).Error; err != nil {
				return err
			}
			for _, c := range canaries {
				found = append(found, finding{r.Kind, fmt.Sprintf("canary:%d", c.ID), fmt.Sprintf("节点组 #%d 灰度发布 %s 已回滚/失败：%s", c.GroupID, c.RevisionID, c.Message)})
			}
		case KindBackupStale:
			maxAge := time.Duration(r.Threshold * float64(time.Hour))
			if maxAge <= 0 {
				maxAge = 24 * time.Hour
			}
			tsStr, err := s.Store.GetKV("last_backup_at")
			if err != nil || tsStr == "" {
				found = append(found, finding{r.Kind, "backup:console", fmt.Sprintf("控制台尚未执行过加密备份（要求每 %.0f 小时至少一次）", maxAge.Hours())})
			} else if lastAt, err := time.Parse(time.RFC3339, tsStr); err != nil || now.Sub(lastAt) >= maxAge {
				found = append(found, finding{r.Kind, "backup:console", fmt.Sprintf("控制台距上次成功备份（%s）已超过 %.0f 小时", tsStr, maxAge.Hours())})
			}
		case KindDiskUsage:
			if s.DataDir != "" || s.DiskUsage != nil {
				usedPct, inodePct, err := s.statDiskUsage()
				if err == nil {
					if usedPct >= r.Threshold {
						found = append(found, finding{r.Kind, "disk:console", fmt.Sprintf("控制台数据目录磁盘使用率 %.1f%% 超过阈值 %.1f%%", usedPct, r.Threshold)})
					} else if inodePct >= r.Threshold {
						found = append(found, finding{r.Kind, "disk:console", fmt.Sprintf("控制台数据目录 inode 使用率 %.1f%% 超过阈值 %.1f%%", inodePct, r.Threshold)})
					}
				}
			}
		}
	}
	ae := q.AlertEvent
	open, err := ae.WithContext(bg).Where(ae.Status.Eq("firing")).Find()
	if err != nil {
		return err
	}
	openBy := map[string]*store.AlertEvent{}
	for _, e := range open {
		openBy[e.Kind+"|"+e.ObjectKey] = e
	}
	seen := map[string]bool{}
	for _, f := range found {
		k := f.kind + "|" + f.key
		seen[k] = true
		if openBy[k] != nil {
			continue
		}
		ev := &store.AlertEvent{Kind: f.kind, ObjectKey: f.key, Message: f.msg, Status: "firing", FiredAt: now}
		ev.Notify = s.notify(enabled[f.kind], Message{Title: "【告警】" + KindName[f.kind], Text: f.msg})
		if err := ae.WithContext(bg).Create(ev); err != nil {
			return err
		}
	}
	for k, e := range openBy {
		if seen[k] {
			continue
		}
		// 规则关闭或条件消失都视为恢复
		res := ""
		if r := enabled[e.Kind]; r != nil {
			res = s.notify(r, Message{Title: "【恢复】" + KindName[e.Kind], Text: e.Message + "（已恢复）", Resolved: true})
		}
		if _, err := ae.WithContext(bg).Where(ae.ID.Eq(e.ID)).UpdateSimple(ae.Status.Value("resolved"), ae.ResolvedAt.Value(now), ae.Notify.Value(res)); err != nil {
			return err
		}
	}
	return nil
}

// notify 发送到规则关联的启用通道，返回结果摘要。
func (s *Service) notify(r *store.AlertRule, m Message) string {
	if r == nil || r.ChannelIDs == "" {
		return "未配置通道"
	}
	var out []string
	for _, idStr := range strings.Split(r.ChannelIDs, ",") {
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			continue
		}
		ac := s.q().AlertChannel
		ch, err := ac.WithContext(bg).Where(ac.ID.Eq(uint(id)), ac.Enabled.Is(true)).First()
		if err != nil {
			continue
		}
		if err := s.Send(ch, m); err != nil {
			out = append(out, ch.Name+"：失败 "+err.Error())
			if s.Log != nil {
				s.Log.Warn("alert notify failed", "channel", ch.Name, "err", err)
			}
		} else {
			out = append(out, ch.Name+"：已发送")
		}
	}
	if len(out) == 0 {
		return "无可用通道"
	}
	return strings.Join(out, "；")
}

// Run 每分钟评估一次，直到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Evaluate(); err != nil && s.Log != nil {
				s.Log.Warn("alert evaluation failed", "err", err)
			}
		}
	}
}

func (s *Service) statDiskUsage() (usedPct, inodePct float64, err error) {
	if s.DiskUsage != nil {
		return s.DiskUsage(s.DataDir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.DataDir, &st); err != nil {
		return 0, 0, err
	}
	if st.Blocks > 0 {
		used := st.Blocks - st.Bavail
		usedPct = float64(used) * 100 / float64(st.Blocks)
	}
	if st.Files > 0 {
		usedInodes := st.Files - st.Ffree
		inodePct = float64(usedInodes) * 100 / float64(st.Files)
	}
	return usedPct, inodePct, nil
}
