package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// sampleRules 覆盖全部原生条件与动作；匹配值里故意放入引号、分号、花括号与 $ 等字符，必须被转义成字面量。
func sampleRules() []Rule {
	return []Rule{
		{Name: "block-office", Conditions: []Condition{{Kind: "ip", CIDRs: []string{"10.66.0.0/16", "192.0.2.7", "2001:db8::/32"}}}, Action: RuleAction{Type: "deny", Status: 403}},
		{Name: "only-cn", Conditions: []Condition{{Kind: "ip_set", Set: "cn", Negate: true}, {Kind: "path", Op: "prefix", Value: "/admin/"}}, Action: RuleAction{Type: "deny", Status: 451}},
		{Name: "bad-bots", Conditions: []Condition{{Kind: "user_agent", Op: "contains", Value: `BadBot"; return 200 "x`}}, Action: RuleAction{Type: "deny", Status: 444}},
		{Name: "need-key", Conditions: []Condition{{Kind: "header", Header: "X-Api-Key", Op: "exists", Negate: true}, {Kind: "method", Methods: []string{"POST", "PUT"}}}, Action: RuleAction{Type: "deny", Status: 401}},
		{Name: "env-test", Conditions: []Condition{{Kind: "header", Header: "X-Env", Op: "regex", Value: `^(test|stage)-[0-9]{1,3}$`}}, Action: RuleAction{Type: "redirect", Status: 302, To: "https://test.example.com/", PreservePath: true}},
		{Name: "weird", Conditions: []Condition{{Kind: "header", Header: "X-Tag", Op: "equals", Value: `a{b};c$host\d`}}, Action: RuleAction{Type: "deny", Status: 403}},
		{Name: "api-limit", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/api/"}}, Action: RuleAction{Type: "rate_limit", Rate: 20, Per: "s", Burst: 40}},
		{Name: "per-tenant", Conditions: []Condition{{Kind: "path", Op: "regex", Value: `^/v[0-9]+/orders`}}, Action: RuleAction{Type: "rate_limit", Rate: 600, Per: "m", Key: "header", KeyHeader: "X-Tenant"}},
	}
}

func rulesInput(t *testing.T) Input {
	in := sampleInput(t)
	in.IPSets = []IPSet{{Name: "cn", CIDRs: []string{"1.0.1.0/24", "1.0.2.0/23"}}, {Name: "unused", CIDRs: []string{"9.9.9.0/24"}}}
	in.Sites[0].Rules = sampleRules()
	return in
}

func TestRulesRender(t *testing.T) {
	plain, err := Render(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.Files["syedge.conf"]), "etos_not_acme") || plain.Files["conf.d/00-ipsets.conf"] != nil {
		t.Fatal("rule scaffolding rendered without rules")
	}

	b, err := Render(rulesInput(t))
	if err != nil {
		t.Fatal(err)
	}
	site := string(b.Files["conf.d/site-shop.conf"])
	for _, want := range []string{
		"geo  $etos_shop_r0_c0 {\n    default  0;\n    10.66.0.0/16  1;\n    192.0.2.7/32  1;\n    2001:db8::/32  1;",
		"map  $etos_set_cn  $etos_shop_r1_c0 {\n    1  0;\n    default  1;", // 取反
		`map  "$etos_not_acme$etos_shop_r1_c0$etos_shop_r1_c1"  $etos_shop_r1 {` + "\n    \"111\"  1;",
		`"~*BadBot\";\\ return\\ 200\\ \"x"  1;`, // 引号按配置语法转义；空格等成为正则字面量
		"map  $http_x_api_key  $etos_shop_r3_c0 {\n    \"\"  1;\n    default  0;",
		`"~^(POST|PUT)$"`,
		`"~^(test|stage)-[0-9]{1,3}$"`,
		`"~*^a\\{b\\};c\\$host\\\\d$"`, // $host 不会被当作变量
		"limit_req_zone  $etos_shop_r6_key  zone=etos_shop_r6:10m  rate=20r/s;",
		"map  $etos_shop_r7  $etos_shop_r7_key {\n    1  $http_x_tenant;",
		"rate=600r/m;",
		"if ($etos_shop_r0) {\n        return  403;",
		"return  302  \"https://test.example.com$request_uri\";",
		"limit_req  zone=etos_shop_r6  burst=40  nodelay;",
		"limit_req  zone=etos_shop_r7;",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site conf lacks %q", want)
		}
	}
	if !strings.Contains(string(b.Files["syedge.conf"]), "$etos_not_acme") {
		t.Error("ACME exemption missing")
	}
	sets := string(b.Files["conf.d/00-ipsets.conf"])
	if !strings.Contains(sets, "geo  $etos_set_cn {") || strings.Contains(sets, "unused") {
		t.Errorf("ip sets:\n%s", sets)
	}
	if strings.Contains(string(b.Files["syedge.conf"]), "load_module") {
		t.Error("native rules must not load Lua")
	}
}

func TestAuthRequestRule(t *testing.T) {
	in := sampleInput(t)
	in.Sites[0].Rules = []Rule{{Name: "sso", Conditions: []Condition{{Kind: "path", Op: "prefix", Value: "/api/"}},
		Action: RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "api", Path: "/auth/verify"}}}}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	site := string(b.Files["conf.d/site-shop.conf"])
	for _, want := range []string{
		"set  $etos_auth_gate  $etos_shop_r0;\n    auth_request  /__etos_auth_shop;",
		"location = /__etos_auth_shop {\n        internal;",
		"proxy_pass        http://up_api/auth/verify;",
		"proxy_set_header  X-Request-ID       $request_id;",
		"proxy_pass_request_body  off;",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site conf lacks %q:\n%s", want, site)
		}
	}
}

func TestRulesValidation(t *testing.T) {
	bad := map[string]func(*Input){
		"bad cidr":        func(in *Input) { in.Sites[0].Rules[0].Conditions[0].CIDRs = []string{"10.0.0.0/33"} },
		"unknown set":     func(in *Input) { in.Sites[0].Rules[1].Conditions[0].Set = "nope" },
		"bad header":      func(in *Input) { in.Sites[0].Rules[3].Conditions[0].Header = "X Api" },
		"bad regex":       func(in *Input) { in.Sites[0].Rules[4].Conditions[0].Value = "(" },
		"control char":    func(in *Input) { in.Sites[0].Rules[2].Conditions[0].Value = "a\nb" },
		"non-ascii":       func(in *Input) { in.Sites[0].Rules[2].Conditions[0].Value = "机器人" },
		"redirect dollar": func(in *Input) { in.Sites[0].Rules[4].Action.To = "https://x.example.com/$host" },
		"redirect scheme": func(in *Input) { in.Sites[0].Rules[4].Action.To = "javascript:alert(1)" },
		"deny status":     func(in *Input) { in.Sites[0].Rules[0].Action.Status = 200 },
		"rate zero":       func(in *Input) { in.Sites[0].Rules[6].Action.Rate = 0 },
		"bad method":      func(in *Input) { in.Sites[0].Rules[3].Conditions[1].Methods = []string{"TRACE"} },
		"dup name":        func(in *Input) { in.Sites[0].Rules[1].Name = in.Sites[0].Rules[0].Name },
		"bad name":        func(in *Input) { in.Sites[0].Rules[0].Name = "Bad Name" },
		"unknown action":  func(in *Input) { in.Sites[0].Rules[0].Action.Type = "exec" },
		"unknown kind":    func(in *Input) { in.Sites[0].Rules[0].Conditions[0].Kind = "lua" },
		"auth no upstream": func(in *Input) {
			in.Sites[0].Rules[0].Action = RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "nope", Path: "/a"}}
		},
		"two auth": func(in *Input) {
			a := RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "api", Path: "/a"}}
			in.Sites[0].Rules[0].Action, in.Sites[0].Rules[1].Action = a, a
		},
		"reserved path": func(in *Input) { in.Sites[1].Routes[0].Path = "/__etos_auth_x" },
		"too many rules": func(in *Input) {
			for i := 0; i <= MaxRules; i++ {
				in.Sites[1].Rules = append(in.Sites[1].Rules, Rule{Name: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Action: RuleAction{Type: "deny", Status: 403}})
			}
		},
		"set bad cidr": func(in *Input) { in.IPSets[0].CIDRs = []string{"nope"} },
	}
	for name, f := range bad {
		in := rulesInput(t)
		f(&in)
		if _, err := Render(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 规则（含刻意构造的特殊字符）必须通过真实 syedge -t：证明转义后的配置语法正确、没有注入。
func TestRulesSyedgeSyntax(t *testing.T) {
	bin := os.Getenv("SYEDGE_BIN")
	if bin == "" {
		bin = filepath.Join(layout.DefaultSystemRoot(), "sbin", "syedge")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syedge binary not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	in := rulesInput(t)
	in.Profile = Profile{Mode: "system", Root: layout.DefaultSystemRoot()}
	if mt, err := os.ReadFile(filepath.Join(layout.DefaultSystemRoot(), "share/defaults/mime.types")); err == nil {
		in.MimeTypes = mt
	}
	out, _ := exec.Command(bin, "-V").CombinedOutput()
	if strings.Contains(string(out), "http_auth_request_module") {
		in.Sites[1].Rules = []Rule{{Name: "sso", Action: RuleAction{Type: "auth_request", Auth: &AuthAction{Upstream: "api", Path: "/auth/verify"}}}}
	} else if os.Getenv("CI") != "" {
		t.Fatal("CI syedge must be built with auth_request")
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), in.Revision)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-p", in.Profile.lay().RuntimeRoot(), "-e", in.Profile.lay().ErrorLog(), "-t", "-q", "-c", filepath.Join(dir, "syedge.conf")).CombinedOutput(); err != nil {
		t.Fatalf("syedge -t failed: %v\n%s", err, out)
	}
}
