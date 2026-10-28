package webtls

import (
	"crypto/ecdsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureCreatesReusesAndRenews(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Dir: dir, Hosts: []string{"console.example.com", "10.0.0.5"}}
	c1, err := m.Ensure(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c1.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("browsers need ECDSA, got %T", leaf.PublicKey)
	}
	if leaf.VerifyHostname("console.example.com") != nil || leaf.VerifyHostname("10.0.0.5") != nil || leaf.VerifyHostname("localhost") != nil {
		t.Fatalf("SANs missing: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "key.pem")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	// 再次启动复用同一张证书
	c2, _ := (&Manager{Dir: dir, Hosts: m.Hosts}).Ensure(time.Now())
	if string(c2.Certificate[0]) != string(c1.Certificate[0]) {
		t.Fatal("certificate regenerated on restart")
	}
	// 主机名变化时重新签发
	c3, _ := (&Manager{Dir: dir, Hosts: []string{"new.example.com"}}).Ensure(time.Now())
	if l3, _ := x509.ParseCertificate(c3.Certificate[0]); l3.VerifyHostname("new.example.com") != nil {
		t.Fatal("not reissued for new hosts")
	}
	// 30 天内到期时续期
	c4, _ := (&Manager{Dir: dir, Hosts: []string{"new.example.com"}}).Ensure(time.Now().Add(340 * 24 * time.Hour))
	if string(c4.Certificate[0]) == string(c3.Certificate[0]) {
		t.Fatal("not renewed near expiry")
	}
	if fp := Fingerprint(c4); len(fp) != 95 { // AA:BB:... 32 字节
		t.Fatalf("fingerprint %q", fp)
	}
}
