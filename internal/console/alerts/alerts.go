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
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

// 规则种类。
const (
	KindNodeOffline = "node_offline" // 阈值：离线分钟数
	KindErrorRate   = "error_rate"   // 阈值：近 5 分钟 5xx 占比（%），请求数不少于 MinRequests 才计算
	KindCertExpiry  = "cert_expiry"  // 阈值：剩余天数
	KindDrift       = "drift"        // 节点配置被手工改动
)

// Defaults 是规则的默认阈值。
var Defaults = map[string]float64{KindNodeOffline: 3, KindErrorRate: 5, KindCertExpiry: 30, KindDrift: 0}

// KindName 是规则的中文名。
var KindName = map[string]string{KindNodeOffline: "节点离线", KindErrorRate: "5xx 错误率", KindCertExpiry: "证书即将到期", KindDrift: "配置被手工改动"}

// MinRequests 是计算 5xx 率的最少请求数（流量太小时比例没有意义）。
const MinRequests = 50

// Service 是告警服务。
type Service struct {
	Store   *store.Store
	Box     *secretbox.Box
	Log     *slog.Logger
	IMAllow func() bool // 授权是否包含 alert_im
	HTTP    *http.Client
	Now     func() time.Time
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
	return &http.Client{Timeout: 10 * time.Second}
}

var bg = context.Background()

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
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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

func (s *Service) mail(ch *store.AlertChannel, pass string, m Message) error {
	u, err := url.Parse(ch.URL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	addr := net.JoinHostPort(host, u.Port())
	to := strings.Split(ch.MailTo, ",")
	for i := range to {
		to[i] = strings.TrimSpace(to[i])
	}
	subject := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(m.Title)) + "?="
	msg := "From: " + ch.MailFrom + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(m.Text)) + "\r\n"
	var conn net.Conn
	d := net.Dialer{Timeout: 10 * time.Second}
	if u.Scheme == "smtps" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
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
