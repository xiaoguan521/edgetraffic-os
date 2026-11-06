package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

var day = 24 * time.Hour

func fixture(t *testing.T) (KeyRing, ed25519.PrivateKey, *Payload, Env) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issued := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	p := &Payload{
		V: 1, KID: "etos-test", LicenseID: "LIC-TEST-1",
		Customer: Customer{ID: "C1", Name: "测试客户"},
		Edition:  EditionProfessional,
		Features: []Feature{FeatureCore, FeatureBatchRelease},
		Limits:   Limits{Nodes: 50, Sites: 2000, AuditDays: 365},
		IssuedAt: issued, NotBefore: issued,
		ExpiresAt:        issued.Add(365 * day),
		MaintenanceUntil: issued.Add(365 * day),
		GraceDays:        15,
		Binding:          Binding{Type: "installation", InstallationID: "inst_abc"},
		Product:          Product{Name: "SyEdge", MinVersion: "1.0.0", MaxVersion: "1.*"},
	}
	env := Env{Now: issued.Add(10 * day), InstallationID: "inst_abc", FingerprintMatch: true, BuildDate: issued}
	return KeyRing{"etos-test": pub}, priv, p, env
}

func mustSign(t *testing.T, p *Payload, k ed25519.PrivateKey) string {
	t.Helper()
	s, err := Sign(p, k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValid(t *testing.T) {
	keys, priv, p, env := fixture(t)
	s := Evaluate(mustSign(t, p, priv), keys, env)
	if s.State != StateValid || !s.Writable() {
		t.Fatalf("state = %s (%s)", s.State, s.Reason)
	}
	if !s.Feature(FeatureBatchRelease) || s.Feature(FeatureGMTLS) {
		t.Fatal("feature gating wrong")
	}
}

func TestTamperedPayload(t *testing.T) {
	keys, priv, p, env := fixture(t)
	raw := mustSign(t, p, priv)
	parts := strings.Split(raw, ".")
	b := []byte(parts[0])
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	if s := Evaluate(string(b)+"."+parts[1], keys, env); s.State != StateInvalid {
		t.Fatalf("tampered payload accepted: %s", s.State)
	}
}

func TestUnknownKID(t *testing.T) {
	keys, priv, p, env := fixture(t)
	p.KID = "revoked-2025"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid {
		t.Fatalf("unknown kid accepted: %s", s.State)
	}
}

func TestOtherInstallation(t *testing.T) {
	keys, priv, p, env := fixture(t)
	env.InstallationID = "inst_other"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid {
		t.Fatalf("other installation accepted: %s", s.State)
	}
}

func TestFingerprint(t *testing.T) {
	keys, priv, p, env := fixture(t)
	env.FingerprintMatch = false
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid {
		t.Fatalf("fingerprint mismatch accepted: %s", s.State)
	}
	p.Binding.Type = "installation_only"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateValid {
		t.Fatalf("installation_only should ignore fingerprint: %s", s.State)
	}
}

func TestLifecycle(t *testing.T) {
	keys, priv, p, env := fixture(t)
	raw := mustSign(t, p, priv)
	cases := []struct {
		at   time.Time
		want State
	}{
		{p.ExpiresAt.Add(-31 * day), StateValid},
		{p.ExpiresAt.Add(-1 * day), StateExpiring},
		{p.ExpiresAt.Add(1 * day), StateGrace},
		{p.ExpiresAt.Add(16 * day), StateExpired},
	}
	for _, c := range cases {
		env.Now = c.at
		s := Evaluate(raw, keys, env)
		if s.State != c.want {
			t.Fatalf("at %s: got %s want %s", c.at, s.State, c.want)
		}
	}
	env.Now = p.ExpiresAt.Add(16 * day)
	if Evaluate(raw, keys, env).Writable() {
		t.Fatal("EXPIRED must be read-only")
	}
}

func TestClockRollback(t *testing.T) {
	keys, priv, p, env := fixture(t)
	env.MaxSeenTime = env.Now
	env.Now = env.Now.Add(-3 * day)
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid {
		t.Fatalf("clock rollback accepted: %s", s.State)
	}
}

func TestMaintenance(t *testing.T) {
	keys, priv, p, env := fixture(t)
	env.BuildDate = p.MaintenanceUntil.Add(day)
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid {
		t.Fatalf("build after maintenance accepted: %s", s.State)
	}
}

func TestUnlicensed(t *testing.T) {
	keys, _, _, env := fixture(t)
	if s := Evaluate("", keys, env); s.State != StateUnlicensed || s.Writable() {
		t.Fatalf("got %s", s.State)
	}
}

// D3（设计 0001 第 11 节）：指定品牌的 License 只对该品牌的控制台有效；未指定品牌的 License 通用。
func TestBrandBinding(t *testing.T) {
	keys, priv, p, env := fixture(t)
	env.Brand = "syedge"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateValid {
		t.Fatalf("universal license: %s (%s)", s.State, s.Reason)
	}
	p.Product.Brand = "syedge"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateValid {
		t.Fatalf("same brand: %s (%s)", s.State, s.Reason)
	}
	p.Product.Brand = "sytest"
	if s := Evaluate(mustSign(t, p, priv), keys, env); s.State != StateInvalid || !strings.Contains(s.Reason, "sytest") {
		t.Fatalf("other brand: %s (%s)", s.State, s.Reason)
	}
	// 签发：品牌写入 payload，格式与 brand.env 一致
	o := IssueOptions{KID: "k", CustomerName: "c", Edition: EditionStandard, Days: 30, Binding: "installation_only",
		Request: ActivationRequest{InstallationID: "inst_x"}, Brand: "syedge", Now: time.Now()}
	bp, err := BuildPayload(o)
	if err != nil || bp.Product.Brand != "syedge" {
		t.Fatalf("payload brand: %+v %v", bp, err)
	}
	o.Brand = "Bad Brand"
	if _, err := BuildPayload(o); err == nil {
		t.Fatal("invalid brand accepted")
	}
}
