// Package webtls 为 Web 控制台自动生成 HTTPS 证书（规格书 9.3：控制台默认启用 TLS）。
//
// 使用独立的自签 ECDSA P-256 证书：控制台内部 CA 使用 Ed25519，主流浏览器不接受 Ed25519 服务端证书。
// 证书保存在数据目录 web-tls/，主机名变化或 30 天内到期时自动重新签发；
// 生产环境建议用 -tls-cert / -tls-key 换成受信任的证书。
package webtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Manager 管理自签证书。
type Manager struct {
	Dir   string
	Hosts []string

	mu  sync.RWMutex
	cur *tls.Certificate
}

func (m *Manager) hosts() []string {
	set := map[string]bool{"localhost": true, "127.0.0.1": true}
	for _, h := range m.Hosts {
		if h = strings.TrimSpace(h); h != "" && h != "0.0.0.0" && h != "::" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func covers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// Ensure 读取或生成证书，并设为当前证书。
func (m *Manager) Ensure(now time.Time) (*tls.Certificate, error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	crtPath, keyPath := filepath.Join(m.Dir, "cert.pem"), filepath.Join(m.Dir, "key.pem")
	hosts := m.hosts()
	if c, err := tls.LoadX509KeyPair(crtPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && now.Add(30*24*time.Hour).Before(leaf.NotAfter) && covers(leaf, hosts) {
			m.set(&c)
			return &c, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: "syapp-console", Organization: []string{"EdgeTraffic OS"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	c := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	m.set(&c)
	return &c, nil
}

func (m *Manager) set(c *tls.Certificate) {
	m.mu.Lock()
	m.cur = c
	m.mu.Unlock()
}

// GetCertificate 供 tls.Config 使用，证书续期后无需重启。
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cur == nil {
		return nil, fmt.Errorf("webtls: no certificate")
	}
	return m.cur, nil
}

// Fingerprint 返回证书 SHA-256 指纹（AA:BB:… 格式），管理员据此确认浏览器提示的证书。
func Fingerprint(c *tls.Certificate) string {
	s := sha256.Sum256(c.Certificate[0])
	parts := make([]string, len(s))
	for i, b := range s {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
