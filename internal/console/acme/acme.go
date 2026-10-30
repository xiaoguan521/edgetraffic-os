// Package acme 实现证书的 ACME 自动签发与续期（规格书 P6-4，RFC 8555 HTTP-01）。
//
// 验证请求由边缘节点在 80 端口应答：挑战写入渲染器生成的 snippets/acme-challenge.conf，
// 以“线上版本 + 只改续期类文件”的方式下发（deploy.RenewalRefresh），授权只读时也可执行，
// agent 会逐行核对挑战片段的格式。控制台需要能访问 CA；内网可配置自建的 ACME 服务与 CA 证书。
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	xacme "golang.org/x/crypto/acme"
	"gorm.io/gen/field"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// DefaultDirectory 是 Let's Encrypt 生产环境。
const DefaultDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// RenewBefore：ACME 证书剩余有效期不足 30 天时自动续期。
const RenewBefore = 30 * 24 * time.Hour

const (
	kvDirectory  = "acme.directory"
	kvEmail      = "acme.email"
	kvAccountKey = "acme.account_key" // secretbox 加密的 PEM
	kvAccountURI = "acme.account_uri"
	kvAccountDir = "acme.account_directory"
)

// Service 负责 ACME 账号、签发与自动续期。
type Service struct {
	Store  *store.Store
	Box    *secretbox.Box
	Certs  *certs.Service
	Deploy *deploy.Service
	Audit  *audit.Logger
	Log    *slog.Logger
	// CAFile 是额外信任的 CA 证书（PEM），用于内网自建的 ACME 服务；为空时只信任系统 CA。
	CAFile string

	mu      sync.Mutex // 同一时间只执行一个签发流程，避免挑战片段互相覆盖
	running map[uint]bool
}

// Settings 是 ACME 设置。
type Settings struct {
	Directory string `json:"directory"`
	Email     string `json:"email"`
	Account   bool   `json:"account_registered"`
}

// GetSettings 返回当前设置。
func (s *Service) GetSettings() Settings {
	dir, _ := s.Store.GetKV(kvDirectory)
	if dir == "" {
		dir = DefaultDirectory
	}
	email, _ := s.Store.GetKV(kvEmail)
	accDir, _ := s.Store.GetKV(kvAccountDir)
	return Settings{Directory: dir, Email: email, Account: accDir == dir}
}

// SaveSettings 保存设置；更换 CA 地址后会在下次签发时重新注册账号。
func (s *Service) SaveSettings(dir, email, user string) error {
	dir, email = strings.TrimSpace(dir), strings.TrimSpace(email)
	if dir == "" {
		dir = DefaultDirectory
	}
	if !strings.HasPrefix(dir, "https://") {
		return errors.New("ACME 服务地址必须以 https:// 开头")
	}
	if email != "" && (!strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n<>")) {
		return errors.New("邮箱格式不正确")
	}
	if err := s.Store.SetKV(kvDirectory, dir); err != nil {
		return err
	}
	if err := s.Store.SetKV(kvEmail, email); err != nil {
		return err
	}
	_ = s.Audit.Write(audit.Entry{Username: user, Action: "acme.settings", ObjectType: "acme", ObjectID: "settings",
		Detail: map[string]string{"directory": dir, "email": email}})
	return nil
}

func (s *Service) httpClient() (*http.Client, error) {
	if s.CAFile == "" {
		return http.DefaultClient, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	b, err := os.ReadFile(s.CAFile)
	if err != nil {
		return nil, fmt.Errorf("ACME CA 证书：%w", err)
	}
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("ACME CA 证书 %s 中没有可用的证书", s.CAFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}, nil
}

// client 返回已注册账号的 ACME 客户端（账号密钥加密保存在数据库）。
func (s *Service) client(ctx context.Context) (*xacme.Client, error) {
	st := s.GetSettings()
	hc, err := s.httpClient()
	if err != nil {
		return nil, err
	}
	var key *ecdsa.PrivateKey
	if sealed, err := s.Store.GetKV(kvAccountKey); err == nil && sealed != "" {
		p, err := s.Box.Open(sealed)
		if err != nil {
			return nil, err
		}
		blk, _ := pem.Decode([]byte(p))
		if blk == nil {
			return nil, errors.New("ACME 账号密钥已损坏")
		}
		if key, err = x509.ParseECPrivateKey(blk.Bytes); err != nil {
			return nil, err
		}
	} else {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, _ := x509.MarshalECPrivateKey(key)
		sealed, err := s.Box.Seal(string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
		if err != nil {
			return nil, err
		}
		if err := s.Store.SetKV(kvAccountKey, sealed); err != nil {
			return nil, err
		}
	}
	c := &xacme.Client{Key: key, DirectoryURL: st.Directory, HTTPClient: hc, UserAgent: "EdgeTraffic-OS"}
	if accDir, _ := s.Store.GetKV(kvAccountDir); accDir != st.Directory {
		acct := &xacme.Account{}
		if st.Email != "" {
			acct.Contact = []string{"mailto:" + st.Email}
		}
		a, err := c.Register(ctx, acct, xacme.AcceptTOS)
		if errors.Is(err, xacme.ErrAccountAlreadyExists) {
			a, err = c.GetReg(ctx, "")
		}
		if err != nil {
			return nil, fmt.Errorf("注册 ACME 账号失败：%w", err)
		}
		_ = s.Store.SetKV(kvAccountURI, a.URI)
		_ = s.Store.SetKV(kvAccountDir, st.Directory)
	}
	return c, nil
}

// OrderView 是返回给前端的签发记录。
type OrderView struct {
	store.AcmeOrder
	Domains []string `json:"domains"`
}

// bg：签发在后台 goroutine 中运行，不绑定请求上下文。
var bg = context.Background()

func (s *Service) q() *query.Query { return query.Use(s.Store.DB) }

func (s *Service) clearChallenges(orderID uint) {
	ac := s.q().AcmeChallenge
	_, _ = ac.WithContext(bg).Where(ac.OrderID.Eq(orderID)).Delete()
}

// Orders 返回最近的签发记录。
func (s *Service) Orders(limit int) []OrderView {
	o := s.q().AcmeOrder
	rows, _ := o.WithContext(bg).Order(o.ID.Desc()).Limit(limit).Find()
	out := make([]OrderView, 0, len(rows))
	for _, r := range rows {
		v := OrderView{AcmeOrder: *r}
		_ = json.Unmarshal([]byte(r.Domains), &v.Domains)
		out = append(out, v)
	}
	return out
}

// groupsFor 找出线上版本中有站点服务这些域名的节点组，并检查每个域名都能被某个组应答。
func (s *Service) groupsFor(domains []string) ([]uint, error) {
	q := s.q()
	sites, err := q.Site.WithContext(bg).Where(q.Site.Enabled.Is(true)).Find()
	if err != nil {
		return nil, err
	}
	live := map[uint]map[string][]byte{}
	served := map[string]bool{}
	seen := map[uint]bool{}
	var groups []uint
	for _, st := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) != nil {
			continue
		}
		files, ok := live[st.GroupID]
		if !ok {
			g, err := store.First[store.NodeGroup](s.Store, st.GroupID)
			if err == nil && g.CurrentRevisionID != "" {
				if rev, err := q.Revision.WithContext(bg).Where(q.Revision.ID.Eq(g.CurrentRevisionID)).First(); err == nil {
					files, _ = render.ReadTarGz(rev.Bundle)
				}
			}
			live[st.GroupID] = files
		}
		if _, ok := files["conf.d/site-"+st.Name+".conf"]; !ok {
			continue // 站点尚未发布，节点上还没有这个域名
		}
		for _, d := range domains {
			for _, sd := range rs.Domains {
				if sd == d {
					served[d] = true
					if !seen[st.GroupID] {
						seen[st.GroupID] = true
						groups = append(groups, st.GroupID)
					}
				}
			}
		}
	}
	for _, d := range domains {
		if !served[d] {
			return nil, fmt.Errorf("域名 %s 没有已发布的站点：ACME 验证需要该域名的 HTTP 请求能到达节点，请先创建并发布使用该域名的站点", d)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups, nil
}

func normalize(domains []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		if strings.Contains(d, "*") {
			return nil, errors.New("HTTP-01 验证不支持通配符域名，请逐个填写域名")
		}
		if err := render.ValidateDomain(d); err != nil {
			return nil, fmt.Errorf("域名 %s 不合法", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("至少填写一个域名")
	}
	if len(out) > 50 {
		return nil, errors.New("一张证书最多 50 个域名")
	}
	return out, nil
}

// Issue 新签发一张证书（名称不能与已有证书重复）。
func (s *Service) Issue(name string, domains []string, user string) (*store.AcmeOrder, error) {
	if err := render.ValidateName(name); err != nil {
		return nil, errors.New("证书名称不合法：只能使用小写字母、数字和连字符")
	}
	c := s.q().Certificate
	n, err := c.WithContext(bg).Where(c.Name.Eq(name)).Count()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, errors.New("同名证书已存在")
	}
	return s.start(name, 0, domains, user)
}

// Renew 为证书库中的 ACME 证书续期（任何授权状态下可用，规格书 8.6）。
func (s *Service) Renew(certID uint, user string) (*store.AcmeOrder, error) {
	c, err := store.First[store.Certificate](s.Store, certID)
	if err != nil {
		return nil, err
	}
	if c.Source != "acme" {
		return nil, errors.New("这张证书不是通过 ACME 签发的，请在证书页手动续期")
	}
	var domains []string
	_ = json.Unmarshal([]byte(c.Domains), &domains)
	return s.start(c.Name, c.ID, domains, user)
}

func (s *Service) start(name string, certID uint, domains []string, user string) (*store.AcmeOrder, error) {
	domains, err := normalize(domains)
	if err != nil {
		return nil, err
	}
	groups, err := s.groupsFor(domains)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.running == nil {
		s.running = map[uint]bool{}
	}
	ao := s.q().AcmeOrder
	pending, err := ao.WithContext(bg).Where(ao.Status.Eq("pending")).Count()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if pending > 0 {
		s.mu.Unlock()
		return nil, errors.New("已有签发正在进行，请等它完成后再试")
	}
	dj, _ := json.Marshal(domains)
	o := &store.AcmeOrder{Name: name, CertID: certID, Domains: string(dj), Status: "pending", Step: "准备", CreatedBy: user}
	if err := ao.WithContext(bg).Create(o); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.running[o.ID] = true
	s.mu.Unlock()
	_ = s.Audit.Write(audit.Entry{Username: user, Action: "certificate.acme_start", ObjectType: "certificate", ObjectID: name,
		Detail: map[string]any{"order": o.ID, "domains": domains, "renew": certID != 0}})
	go s.run(o, domains, groups, user)
	return o, nil
}

func (s *Service) step(o *store.AcmeOrder, step string) {
	o.Step = step
	ao := s.q().AcmeOrder
	_, _ = ao.WithContext(bg).Where(ao.ID.Eq(o.ID)).Update(ao.Step, step)
}

// publish 把当前挑战同步到这些节点组并等待生效。
func (s *Service) publish(ctx context.Context, groups []uint, certID uint) error {
	for _, g := range groups {
		var dep *store.Deployment
		var err error
		for try := 0; try < 24; try++ { // 组内正在发布时最多等 2 分钟
			if dep, err = s.Deploy.RenewalRefresh(g, certID, "system:acme", ""); !errors.Is(err, deploy.ErrBusy) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if errors.Is(err, deploy.ErrNeedsPublish) {
			return errors.New("节点组的线上版本早于 ACME 支持，请先在“发布”页发布一次")
		}
		if err != nil {
			return err
		}
		if dep == nil {
			continue
		}
		done, err := s.Deploy.Wait(ctx, dep.ID)
		if err != nil {
			return err
		}
		if done.Status != "APPLIED" {
			return fmt.Errorf("节点组 %d 下发验证文件未全部生效（%s），请查看发布记录", g, done.Status)
		}
	}
	return nil
}

func (s *Service) run(o *store.AcmeOrder, domains []string, groups []uint, user string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	defer func() {
		s.mu.Lock()
		delete(s.running, o.ID)
		s.mu.Unlock()
	}()
	fail := func(err error) {
		s.clearChallenges(o.ID)
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer ccancel()
		_ = s.publish(cctx, groups, 0) // 清理节点上的挑战片段
		now := time.Now()
		ao := s.q().AcmeOrder
		_, _ = ao.WithContext(bg).Where(ao.ID.Eq(o.ID)).UpdateSimple(ao.Status.Value("failed"), ao.Error.Value(err.Error()), ao.FinishedAt.Value(now))
		_ = s.Audit.Write(audit.Entry{Username: user, Action: "certificate.acme_result", ObjectType: "certificate", ObjectID: o.Name,
			Result: "failure", Detail: map[string]any{"order": o.ID, "step": o.Step, "error": err.Error()}})
		if s.Log != nil {
			s.Log.Warn("acme order failed", "order", o.ID, "name", o.Name, "step", o.Step, "err", err)
		}
	}

	s.step(o, "连接 ACME 服务")
	c, err := s.client(ctx)
	if err != nil {
		fail(err)
		return
	}
	s.step(o, "创建订单")
	order, err := c.AuthorizeOrder(ctx, xacme.DomainIDs(domains...))
	if err != nil {
		fail(fmt.Errorf("创建订单失败：%w", err))
		return
	}
	var chals []*xacme.Challenge
	for _, u := range order.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			fail(err)
			return
		}
		if z.Status == xacme.StatusValid {
			continue
		}
		var ch *xacme.Challenge
		for _, x := range z.Challenges {
			if x.Type == "http-01" {
				ch = x
			}
		}
		if ch == nil {
			fail(fmt.Errorf("CA 没有为 %s 提供 HTTP-01 验证", z.Identifier.Value))
			return
		}
		ka, err := c.HTTP01ChallengeResponse(ch.Token)
		if err != nil {
			fail(err)
			return
		}
		if err := s.q().AcmeChallenge.WithContext(bg).Create(&store.AcmeChallenge{OrderID: o.ID, Domain: z.Identifier.Value, Token: ch.Token, KeyAuth: ka}); err != nil {
			fail(err)
			return
		}
		chals = append(chals, ch)
	}
	if len(chals) > 0 {
		s.step(o, "下发验证文件到节点")
		if err := s.publish(ctx, groups, 0); err != nil {
			fail(err)
			return
		}
		s.step(o, "等待 CA 验证")
		for _, ch := range chals {
			if _, err := c.Accept(ctx, ch); err != nil {
				fail(fmt.Errorf("提交验证失败：%w", err))
				return
			}
		}
		for _, u := range order.AuthzURLs {
			if _, err := c.WaitAuthorization(ctx, u); err != nil {
				fail(fmt.Errorf("CA 验证未通过：%w", err))
				return
			}
		}
	}
	s.step(o, "签发证书")
	if order, err = c.WaitOrder(ctx, order.URI); err != nil {
		fail(err)
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fail(err)
		return
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains}, key)
	if err != nil {
		fail(err)
		return
	}
	// CreateCertFromOrder 显式带上订单地址；旧的 CreateOrderCert 依赖 finalize 响应里的 Location 头，
	// RFC 8555 并不要求 CA 返回它（Pebble 等就不返回），会导致轮询空地址。
	chain, _, err := c.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		fail(fmt.Errorf("签发失败：%w", err))
		return
	}
	var certPEM strings.Builder
	for _, der := range chain {
		_ = pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))

	s.step(o, "写入证书库并下发")
	s.clearChallenges(o.ID)
	certID := o.CertID
	if certID == 0 {
		cert, err := s.Certs.CreateFrom("acme", o.Name, certPEM.String(), keyPEM, "system:acme")
		if err != nil {
			fail(err)
			return
		}
		certID = cert.ID
	} else if _, err := s.Certs.Renew(certID, certPEM.String(), keyPEM, "system:acme"); err != nil {
		fail(err)
		return
	}
	// 一次下发：替换证书文件并清空挑战片段
	all := map[uint]bool{}
	for _, g := range groups {
		all[g] = true
	}
	for _, g := range s.Certs.Groups(certID) {
		all[g] = true
	}
	var targets []uint
	for g := range all {
		targets = append(targets, g)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	if err := s.publish(ctx, targets, certID); err != nil {
		// 证书已经签发并入库，只是下发失败：记录错误，可在证书页“重新下发”
		ao := s.q().AcmeOrder
		_, _ = ao.WithContext(bg).Where(ao.ID.Eq(o.ID)).UpdateSimple(ao.Status.Value("valid"), ao.CertID.Value(certID),
			ao.Error.Value("证书已签发，下发到节点时出错："+err.Error()), ao.FinishedAt.Value(time.Now()))
		return
	}
	ao := s.q().AcmeOrder
	_, _ = ao.WithContext(bg).Where(ao.ID.Eq(o.ID)).UpdateSimple(ao.Status.Value("valid"), ao.CertID.Value(certID),
		ao.Step.Value("完成"), ao.FinishedAt.Value(time.Now()))
	_ = s.Audit.Write(audit.Entry{Username: user, Action: "certificate.acme_result", ObjectType: "certificate", ObjectID: o.Name,
		Detail: map[string]any{"order": o.ID, "cert_id": certID, "domains": domains}})
}

// Due 返回需要自动续期的 ACME 证书：剩余不足 30 天、开启自动续期、6 小时内没有失败过。
func (s *Service) Due(now time.Time) []store.Certificate {
	q := s.q()
	c, ao := q.Certificate, q.AcmeOrder
	cs, _ := c.WithContext(bg).Where(c.Source.Eq("acme"), c.AutoRenew.Is(true), c.NotAfter.Lt(now.Add(RenewBefore))).Find()
	var out []store.Certificate
	for _, c := range cs {
		// cert_id = ? AND (status = 'pending' OR (status = 'failed' AND created_at > 6 小时前))
		recent, err := ao.WithContext(bg).Where(ao.CertID.Eq(c.ID), field.Or(ao.Status.Eq("pending"),
			field.And(ao.Status.Eq("failed"), ao.CreatedAt.Gt(now.Add(-6*time.Hour))))).Count()
		if err == nil && recent == 0 {
			out = append(out, *c)
		}
	}
	return out
}

// Loop 每小时检查一次需要续期的证书；启动时把上次异常退出遗留的进行中订单标记为失败。
func (s *Service) Loop(ctx context.Context) {
	ao, ac := s.q().AcmeOrder, s.q().AcmeChallenge
	_, _ = ao.WithContext(ctx).Where(ao.Status.Eq("pending")).
		UpdateSimple(ao.Status.Value("failed"), ao.Error.Value("控制台重启，签发中断"), ao.FinishedAt.Value(time.Now()))
	_, _ = ac.WithContext(ctx).Where(ac.ID.Gt(0)).Delete()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		for _, c := range s.Due(time.Now()) {
			if _, err := s.Renew(c.ID, "system:acme"); err != nil && s.Log != nil {
				s.Log.Warn("acme auto renew not started", "cert", c.Name, "err", err)
			}
			break // 一次只启动一张，其余下个周期处理
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
