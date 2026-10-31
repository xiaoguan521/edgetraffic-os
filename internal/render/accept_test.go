package render

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAcceptanceConfig 为 CI 的 P10 验收（scripts/ci/lua-acceptance.sh）生成版本目录：用真实渲染器产出配置，
// 写到 /opt/syapp/conf/releases/<ACCEPT_REV> 并通过真实 syapp -t。只在设置 ACCEPT_REV 时运行（需要 root）。
//
//	ACCEPT_LUA=1        包含 JWT（Lua）规则；=0 生成不含 Lua 的版本（卸载附加包前切换用）
//	ACCEPT_HS_SECRET    HS256 密钥；ACCEPT_RS_PUB  RS256 公钥 PEM 文件
//	ACCEPT_GM=<目录>    国密基础包：sign.crt/sign.key/enc.crt/enc.key（站点 api 同时启用国密双证书）
func TestAcceptanceConfig(t *testing.T) {
	rev := os.Getenv("ACCEPT_REV")
	if rev == "" {
		t.Skip("ACCEPT_REV not set")
	}
	lua := os.Getenv("ACCEPT_LUA") == "1"
	proxy := []Route{{Path: "/", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "backend"}}}
	ipDeny := Rule{Name: "ip-deny", Conditions: []Condition{{Kind: "ip", CIDRs: []string{"10.66.0.0/16"}}}, Action: RuleAction{Type: "deny", Status: 403}}
	bots := Rule{Name: "bots", Conditions: []Condition{{Kind: "user_agent", Op: "contains", Value: "BadBot"}}, Action: RuleAction{Type: "deny", Status: 403}}
	api := Site{Name: "api", Domains: []string{"api.test"}, HTTPS: selfSigned(t, "api.test"), Routes: proxy, Rules: []Rule{
		ipDeny, bots,
		{Name: "old", Conditions: []Condition{{Kind: "path", Op: "exact", Value: "/old"}}, Action: RuleAction{Type: "redirect", Status: 302, To: "https://new.test/", PreservePath: true}},
		{Name: "limited", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/limited/"}}, Action: RuleAction{Type: "rate_limit", Rate: 5, Per: "s"}},
		{Name: "auth", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/auth/"}}, Action: RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "authsvc", Path: "/check"}}},
	}}
	bench3 := Site{Name: "bench3", Domains: []string{"bench3.test"}, Routes: proxy, Rules: []Rule{ipDeny, bots}}
	// 诊断用：只有两条原生规则 / 只有 JWT（拆分三条规则的开销）
	benchN := Site{Name: "benchn", Domains: []string{"benchn.test"}, Routes: proxy, Rules: []Rule{ipDeny, bots}}
	benchJ := Site{Name: "benchj", Domains: []string{"benchj.test"}, Routes: proxy}
	if lua {
		hs := os.Getenv("ACCEPT_HS_SECRET")
		pub, err := os.ReadFile(os.Getenv("ACCEPT_RS_PUB"))
		if err != nil {
			t.Fatal(err)
		}
		api.Rules = append(api.Rules,
			Rule{Name: "hs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/hs/"}},
				Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hs, Source: "header", Issuer: "etos-ci", Audience: "api", SubjectHeader: "X-User"}}},
			Rule{Name: "rs", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/rs/"}},
				Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "RS256", PublicKey: string(pub), Source: "header"}}},
		)
		// 压测站点的“三条常用规则”：IP 拒绝、UA 拒绝（都不命中）+ 对所有请求的 JWT 校验
		bench3.Rules = append(bench3.Rules, Rule{Name: "jwt", Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hs, Source: "header"}}})
		benchJ.Rules = []Rule{{Name: "jwt", Action: RuleAction{Type: "jwt", JWT: &JWTAction{Alg: "HS256", Secret: hs, Source: "header"}}}}
	}
	if gm := os.Getenv("ACCEPT_GM"); gm != "" {
		rd := func(n string) string {
			b, err := os.ReadFile(filepath.Join(gm, n))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		api.HTTPS.GM = &GMPair{SignCertPEM: rd("sign.crt"), SignKeyPEM: rd("sign.key"), EncCertPEM: rd("enc.crt"), EncKeyPEM: rd("enc.key")}
	}
	in := Input{
		Revision:   rev,
		AllowedNet: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RealIPFrom: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		MimeTypes:  DefaultMimeTypes(),
		Upstreams: []UpstreamGroup{
			{Name: "backend", Servers: []UpstreamServer{{Addr: "10.99.0.1:18080"}}},
			{Name: "authsvc", Servers: []UpstreamServer{{Addr: "10.99.0.1:18081"}}},
		},
		Sites: []Site{api, {Name: "bench0", Domains: []string{"bench0.test"}, Routes: proxy}, bench3, benchN, benchJ},
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ReleasesDir, rev)
	_ = os.RemoveAll(dir)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	bin := os.Getenv("SYAPP_BIN")
	if bin == "" {
		bin = "/opt/syapp/sbin/syapp"
	}
	if out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syapp.conf")).CombinedOutput(); err != nil {
		t.Fatalf("syapp -t failed: %v\n%s", err, out)
	}
}
