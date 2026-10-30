package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

const testSecret = "0123456789abcdef0123456789abcdef-secret"

func jwtSite(secret string) render.Site {
	return render.Site{Name: "api", Domains: []string{"api.example.com"},
		Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}},
		Rules: []render.Rule{
			{Name: "office", Conditions: []render.Condition{{Kind: "ip_set", Set: "office"}}, Action: render.RuleAction{Type: "deny", Status: 403}},
			{Name: "api-jwt", Conditions: []render.Condition{{Kind: "path", Op: "prefix", Value: "/v1/"}},
				Action: render.RuleAction{Type: "jwt", JWT: &render.JWTAction{Alg: "HS256", Secret: secret, Source: "header"}}},
		}}
}

func TestRulesSecretsAndLuaGate(t *testing.T) {
	d, g := newTestService(t)
	d.Box, _ = secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	licensed := false
	d.Feature = func(f string) bool { return f != "lua" || licensed }
	q := query.Use(d.Store.DB)
	ctx := context.Background()

	// 保存：明文密钥加密后清空；编辑时不填密钥沿用旧值；响应中不回显
	site := jwtSite(testSecret)
	if err := d.SealRules(&site, nil); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(site)
	if strings.Contains(string(spec), testSecret) || !strings.Contains(string(spec), "secret_enc") {
		t.Fatalf("stored spec: %s", spec)
	}
	edited := jwtSite("")
	if err := d.SealRules(&edited, &site); err != nil || edited.Rules[1].Action.JWT.SecretEnc != site.Rules[1].Action.JWT.SecretEnc {
		t.Fatalf("secret not kept on edit: %v", err)
	}
	view := site
	view.Rules = append([]render.Rule(nil), site.Rules...)
	RedactRules(&view)
	if j := view.Rules[1].Action.JWT; j.SecretEnc != "" || j.Secret != "" || !j.HasSecret {
		t.Fatalf("redacted %+v", j)
	}
	if site.Rules[1].Action.JWT.SecretEnc == "" {
		t.Fatal("redaction modified the stored spec")
	}
	_ = q.Site.WithContext(ctx).Create(&store.Site{GroupID: g.ID, Name: "api", Spec: string(spec), Enabled: true})
	cidrs, _ := json.Marshal([]string{"10.20.0.0/16"})
	_ = q.IPSet.WithContext(ctx).Create(&store.IPSet{GroupID: g.ID, Name: "office", CIDRs: string(cidrs), Count: 1, Source: "manual"})

	// 授权与节点门禁
	if _, _, err := d.Preview(g.ID); err == nil || !strings.Contains(err.Error(), "LICENSE_FEATURE_DENIED") {
		t.Fatalf("unlicensed: %v", err)
	}
	licensed = true
	n := &store.Node{GroupID: g.ID, Name: "edge-1", Status: "online", Arch: "amd64", TLSLib: "openssl"}
	_ = q.Node.WithContext(ctx).Create(n)
	if _, _, err := d.Preview(g.ID); err == nil || !strings.Contains(err.Error(), "LUA_ADDON_REQUIRED") {
		t.Fatalf("no add-on: %v", err)
	}
	_, _ = q.Node.WithContext(ctx).Where(q.Node.ID.Eq(n.ID)).Update(q.Node.LuaAddon, "1.30.6/gmssl")
	if _, _, err := d.Preview(g.ID); err == nil || !strings.Contains(err.Error(), "LUA_ADDON_REQUIRED") {
		t.Fatalf("add-on for the wrong base accepted: %v", err)
	}
	_, _ = q.Node.WithContext(ctx).Where(q.Node.ID.Eq(n.ID)).Update(q.Node.LuaAddon, "1.30.6/standard")
	files, _, err := d.Preview(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawKey, sawSet bool
	for _, f := range files {
		if f.Path == "lua/jwt-api-api-jwt.key" {
			sawKey = true
			if strings.Contains(f.Diff, testSecret) {
				t.Fatal("secret shown in preview")
			}
		}
		if f.Path == "conf.d/00-ipsets.conf" && strings.Contains(f.Diff, "10.20.0.0/16") {
			sawSet = true
		}
	}
	if !sawKey || !sawSet {
		t.Fatalf("preview missing key file or ip set: %+v", files)
	}

	// 升级：装有附加包的节点必须同时有目标版本的附加包，并随升级指令下发其签名
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d.PackageKeys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	imp := func(name, variant string) {
		data := []byte(name + "-1.30.7-1")
		sig, _ := pkgsig.Sign(priv, "etos-ci-pkg-test", name, "1.30.7-1", "amd64", variant, data)
		sb, _ := json.Marshal(sig)
		if _, err := d.ImportPackage(data, sb, "admin", ""); err != nil {
			t.Fatal(err)
		}
	}
	imp("syapp", "standard")
	fa := &luaAgents{fakeAgents: fakeAgents{online: map[uint]bool{n.ID: true}}}
	d.Agents = fa
	if _, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", ""); err == nil || !strings.Contains(err.Error(), "syapp-lua") {
		t.Fatalf("upgrade without the add-on package: %v", err)
	}
	imp("syapp-lua", "lua")
	up, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if v := waitUpgrade(t, d, up.ID); v.Status != "done" {
		t.Fatalf("upgrade %s", v.Status)
	}
	var ls pkgsig.Sig
	if json.Unmarshal(fa.luaSig, &ls) != nil || ls.Name != "syapp-lua" || ls.Version != "1.30.7-1" {
		t.Fatalf("add-on signature not sent: %s", fa.luaSig)
	}
}

type luaAgents struct {
	fakeAgents
	mu     sync.Mutex
	luaSig []byte
}

func (f *luaAgents) Send(ctx context.Context, id uint, m *agentv1.ServerMessage, to time.Duration) (*agentv1.CommandResult, error) {
	if u := m.GetUpgrade(); u != nil && u.Action == agentv1.UpgradeDataplane_START {
		f.mu.Lock()
		f.luaSig = u.LuaSignatureJson
		f.mu.Unlock()
	}
	return f.fakeAgents.Send(ctx, id, m, to)
}
