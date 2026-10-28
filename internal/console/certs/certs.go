// Package certs 是控制台的证书库（规格书 7.3 证书管理、9.3 私钥保护）。
//
// 站点只引用证书编号；证书与私钥在这里统一校验、加密保存和续期。
// 续期在任何授权状态下都可用（规格书 8.6），由调用方负责把新证书下发到节点。
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// ExpiringWindow：剩余有效期不足 30 天视为即将到期。
const ExpiringWindow = 30 * 24 * time.Hour

// Service 管理证书库。
type Service struct {
	Store *store.Store
	Box   *secretbox.Box
	Audit *audit.Logger
}

// View 是返回给前端的证书信息（不含私钥）。
type View struct {
	store.Certificate
	Domains  []string `json:"domains"`
	DaysLeft int      `json:"days_left"`
	Status   string   `json:"status"` // valid | expiring | expired
	Sites    int      `json:"sites"`
	SiteList []string `json:"site_list"`
}

type parsed struct {
	leaf    *x509.Certificate
	chain   string
	key     string
	domains []string
}

// parse 校验证书链与私钥：PEM 格式、私钥与证书匹配、证书未过期。
func parse(certPEM, keyPEM string, now time.Time) (*parsed, error) {
	certPEM, keyPEM = strings.TrimSpace(certPEM)+"\n", strings.TrimSpace(keyPEM)+"\n"
	var chain []*x509.Certificate
	rest := []byte(certPEM)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("证书文件中出现了非证书内容（%s）", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("证书无法解析：%v", err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return nil, errors.New("没有找到 PEM 格式的证书（-----BEGIN CERTIFICATE-----）")
	}
	leaf := chain[0]
	kb, _ := pem.Decode([]byte(keyPEM))
	if kb == nil || !strings.Contains(kb.Type, "PRIVATE KEY") {
		return nil, errors.New("没有找到 PEM 格式的私钥（-----BEGIN PRIVATE KEY-----）")
	}
	if kb.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(keyPEM, "ENCRYPTED") {
		return nil, errors.New("私钥带有密码保护，请先去掉密码再上传")
	}
	priv, err := parseKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("私钥无法解析：%v", err)
	}
	if !samePublic(priv, leaf.PublicKey) {
		return nil, errors.New("证书与私钥不匹配")
	}
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("证书已于 %s 过期", leaf.NotAfter.Format("2006-01-02"))
	}
	domains := append([]string(nil), leaf.DNSNames...)
	if len(domains) == 0 && leaf.Subject.CommonName != "" {
		domains = []string{leaf.Subject.CommonName}
	}
	if len(domains) == 0 {
		return nil, errors.New("证书中没有任何域名")
	}
	return &parsed{leaf: leaf, chain: certPEM, key: keyPEM, domains: domains}, nil
}

func parseKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
		return nil, errors.New("unsupported key type")
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, errors.New("not a PKCS#8, PKCS#1 or EC private key")
}

func samePublic(priv crypto.Signer, pub any) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	switch p := priv.Public().(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		e, ok := p.(equaler)
		return ok && e.Equal(pub)
	}
	return false
}

func fingerprint(c *x509.Certificate) string {
	s := sha256.Sum256(c.Raw)
	return hex.EncodeToString(s[:])
}

func (s *Service) fill(c *store.Certificate, p *parsed, user string) error {
	enc, err := s.Box.Seal(p.key)
	if err != nil {
		return err
	}
	dj, _ := json.Marshal(p.domains)
	c.Domains, c.CertPEM, c.KeyEnc = string(dj), p.chain, enc
	c.Subject, c.Issuer = p.leaf.Subject.CommonName, p.leaf.Issuer.CommonName
	if c.Issuer == "" && len(p.leaf.Issuer.Organization) > 0 {
		c.Issuer = p.leaf.Issuer.Organization[0]
	}
	c.Serial = p.leaf.SerialNumber.Text(16)
	c.NotBefore, c.NotAfter = p.leaf.NotBefore.UTC(), p.leaf.NotAfter.UTC()
	c.Fingerprint, c.UpdatedBy = fingerprint(p.leaf), user
	return nil
}

func (s *Service) audit(user, action string, c *store.Certificate, extra map[string]any) {
	if s.Audit == nil {
		return
	}
	d := map[string]any{"fingerprint": c.Fingerprint, "not_after": c.NotAfter.Format("2006-01-02")}
	for k, v := range extra {
		d[k] = v
	}
	_ = s.Audit.Write(audit.Entry{Username: user, Action: action, ObjectType: "certificate", ObjectID: c.Name, Detail: d})
}

// Create 上传一张新证书。
func (s *Service) Create(name, certPEM, keyPEM, user string) (*store.Certificate, error) {
	return s.CreateFrom("upload", name, certPEM, keyPEM, user)
}

// CreateFrom 写入一张新证书并记录来源；ACME 签发的证书默认开启自动续期。
func (s *Service) CreateFrom(source, name, certPEM, keyPEM, user string) (*store.Certificate, error) {
	if err := render.ValidateName(name); err != nil {
		return nil, fmt.Errorf("证书名称不合法：只能使用小写字母、数字和连字符")
	}
	p, err := parse(certPEM, keyPEM, time.Now())
	if err != nil {
		return nil, err
	}
	c := &store.Certificate{Name: name, Source: source, AutoRenew: source == "acme"}
	if err := s.fill(c, p, user); err != nil {
		return nil, err
	}
	if err := s.Store.DB.Create(c).Error; err != nil {
		return nil, fmt.Errorf("同名证书已存在")
	}
	s.audit(user, "certificate.create", c, map[string]any{"domains": p.domains, "source": source})
	return c, nil
}

// Renew 用新证书替换已有证书（编号不变，引用它的站点自动使用新证书）。
// 新证书必须仍覆盖所有引用站点的域名，否则拒绝，避免续期导致站点不可用。
func (s *Service) Renew(id uint, certPEM, keyPEM, user string) (*store.Certificate, error) {
	c, err := store.First[store.Certificate](s.Store, id)
	if err != nil {
		return nil, err
	}
	p, err := parse(certPEM, keyPEM, time.Now())
	if err != nil {
		return nil, err
	}
	for _, u := range s.users(id) {
		for _, d := range u.domains {
			if p.leaf.VerifyHostname(d) != nil {
				return nil, fmt.Errorf("新证书不包含站点 %s 使用的域名 %s", u.name, d)
			}
		}
	}
	old := c.Fingerprint
	if err := s.fill(c, p, user); err != nil {
		return nil, err
	}
	if err := s.Store.DB.Save(c).Error; err != nil {
		return nil, err
	}
	s.audit(user, "certificate.renew", c, map[string]any{"previous_fingerprint": old})
	return c, nil
}

// Delete 删除未被任何站点引用的证书。
func (s *Service) Delete(id uint, user string) error {
	c, err := store.First[store.Certificate](s.Store, id)
	if err != nil {
		return err
	}
	if us := s.users(id); len(us) > 0 {
		names := make([]string, 0, len(us))
		for _, u := range us {
			names = append(names, u.name)
		}
		return fmt.Errorf("证书正被站点使用：%s", strings.Join(names, "、"))
	}
	if err := s.Store.DB.Delete(c).Error; err != nil {
		return err
	}
	s.audit(user, "certificate.delete", c, nil)
	return nil
}

// Resolve 返回证书链与私钥 PEM，只供渲染使用。
func (s *Service) Resolve(id uint) (string, string, error) {
	c, err := store.First[store.Certificate](s.Store, id)
	if err != nil {
		return "", "", fmt.Errorf("证书 %d 不存在", id)
	}
	key, err := s.Box.Open(c.KeyEnc)
	if err != nil {
		return "", "", fmt.Errorf("证书 %s 的私钥无法解密：%v", c.Name, err)
	}
	return c.CertPEM, key, nil
}

// Covers 检查证书是否覆盖全部域名。
func (s *Service) Covers(id uint, domains []string) error {
	c, err := store.First[store.Certificate](s.Store, id)
	if err != nil {
		return fmt.Errorf("证书 %d 不存在", id)
	}
	blk, _ := pem.Decode([]byte(c.CertPEM))
	if blk == nil {
		return errors.New("证书库中的证书已损坏")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return err
	}
	for _, d := range domains {
		if leaf.VerifyHostname(d) != nil {
			return fmt.Errorf("证书 %s 不包含域名 %s", c.Name, d)
		}
	}
	return nil
}

// ResolveSite 把站点里的证书引用解析为 PEM（渲染前调用），并检查域名覆盖。
func (s *Service) ResolveSite(site *render.Site) error {
	if site.HTTPS == nil {
		return nil
	}
	if site.HTTPS.CertID == 0 {
		return errors.New("启用 HTTPS 时必须从证书库选择证书")
	}
	if err := s.Covers(site.HTTPS.CertID, site.Domains); err != nil {
		return err
	}
	crt, key, err := s.Resolve(site.HTTPS.CertID)
	if err != nil {
		return err
	}
	site.HTTPS.CertPEM, site.HTTPS.KeyPEM = crt, key
	return nil
}

type user struct {
	name    string
	group   uint
	enabled bool
	domains []string
}

// users 列出引用某张证书的站点。
func (s *Service) users(id uint) []user {
	var sites []store.Site
	s.Store.DB.Order("name").Find(&sites)
	var out []user
	for _, st := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) != nil || rs.HTTPS == nil || rs.HTTPS.CertID != id {
			continue
		}
		out = append(out, user{name: st.Name, group: st.GroupID, enabled: st.Enabled, domains: rs.Domains})
	}
	return out
}

// Groups 返回有启用站点引用该证书的节点组，续期后需要向这些组下发新证书。
func (s *Service) Groups(id uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, u := range s.users(id) {
		if u.enabled && !seen[u.group] {
			seen[u.group] = true
			out = append(out, u.group)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SitesUsing 返回某节点组中引用该证书的启用站点名。
func (s *Service) SitesUsing(id, group uint) []string {
	var out []string
	for _, u := range s.users(id) {
		if u.enabled && u.group == group {
			out = append(out, u.name)
		}
	}
	return out
}

func toView(c store.Certificate, us []user, now time.Time) View {
	v := View{Certificate: c}
	_ = json.Unmarshal([]byte(c.Domains), &v.Domains)
	left := c.NotAfter.Sub(now)
	v.DaysLeft = int(left.Hours() / 24)
	switch {
	case left <= 0:
		v.Status, v.DaysLeft = "expired", 0
	case left <= ExpiringWindow:
		v.Status = "expiring"
	default:
		v.Status = "valid"
	}
	v.Sites = len(us)
	v.SiteList = []string{}
	for _, u := range us {
		v.SiteList = append(v.SiteList, u.name)
	}
	return v
}

// List 返回全部证书，按到期时间升序。
func (s *Service) List() ([]View, error) {
	var cs []store.Certificate
	if err := s.Store.DB.Order("not_after").Find(&cs).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]View, 0, len(cs))
	for _, c := range cs {
		out = append(out, toView(c, s.users(c.ID), now))
	}
	return out, nil
}

// Counts 返回即将到期与已过期的证书数量（总览页使用）。
func (s *Service) Counts() (expiring, expired int64) {
	now := time.Now()
	s.Store.DB.Model(&store.Certificate{}).Where("not_after <= ?", now).Count(&expired)
	s.Store.DB.Model(&store.Certificate{}).Where("not_after > ? AND not_after <= ?", now, now.Add(ExpiringWindow)).Count(&expiring)
	return
}

// MigrateInline 把旧版本站点里内嵌的 PEM 迁移到证书库（启动时执行，幂等）。
func (s *Service) MigrateInline() error {
	var sites []store.Site
	if err := s.Store.DB.Find(&sites).Error; err != nil {
		return err
	}
	for _, st := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) != nil || rs.HTTPS == nil || rs.HTTPS.CertID != 0 || rs.HTTPS.CertPEM == "" {
			continue
		}
		p, err := parse(rs.HTTPS.CertPEM, rs.HTTPS.KeyPEM, time.Time{}) // 迁移时不因过期而失败
		if err != nil {
			return fmt.Errorf("site %s: %w", st.Name, err)
		}
		name := "site-" + st.Name
		for i := 2; ; i++ {
			var n int64
			s.Store.DB.Model(&store.Certificate{}).Where("name = ?", name).Count(&n)
			if n == 0 {
				break
			}
			name = fmt.Sprintf("site-%s-%d", st.Name, i)
		}
		c := &store.Certificate{Name: name, Source: "migrated"}
		if err := s.fill(c, p, "system"); err != nil {
			return err
		}
		if err := s.Store.DB.Create(c).Error; err != nil {
			return err
		}
		rs.HTTPS = &render.TLS{CertID: c.ID}
		spec, _ := json.Marshal(rs)
		if err := s.Store.DB.Model(&store.Site{}).Where("id = ?", st.ID).Update("spec", string(spec)).Error; err != nil {
			return err
		}
		s.audit("system", "certificate.migrate", c, map[string]any{"site": st.Name})
	}
	return nil
}
