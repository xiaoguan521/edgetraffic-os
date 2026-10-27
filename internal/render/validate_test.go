package render

import (
	"net/netip"
	"testing"
)

func TestValidateDomain(t *testing.T) {
	ok := []string{"www.example.com", "*.example.com", "a-b.example.cn", "xn--fiqs8s.cn"}
	bad := []string{"", "*", "*.com", "example", "WWW.example.com", "a..com", "-a.com", "a.com;", "a.com\n", "*.*.example.com", "exa mple.com"}
	for _, d := range ok {
		if err := ValidateDomain(d); err != nil {
			t.Errorf("%q rejected: %v", d, err)
		}
	}
	for _, d := range bad {
		if err := ValidateDomain(d); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
}

// 规格书 6.5 渲染器必测用例（路径部分）。
func TestValidatePath(t *testing.T) {
	ok := []string{"/", "/api/", "/static/img", "/a-b_c.d/"}
	bad := []string{
		"", "api", "/api/..%2f..%2fetc/passwd", "/../etc", "/a/./b", "/a/%2e%2e/b",
		"/a\nreturn 200", "/a;", "/a{", "/a}", "/a'", "/a\"", "/a$host", "/a\\b", "/a%0a",
	}
	for _, p := range ok {
		if err := ValidatePath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestValidateUpstream(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if err := ValidateUpstream("10.0.1.11:8080", allowed); err != nil {
		t.Fatal(err)
	}
	bad := []string{"169.254.169.254:80", "127.0.0.1:8080", "192.168.1.1:80", "10.0.0.1:0", "example.com:80", "10.0.0.1"}
	for _, u := range bad {
		if err := ValidateUpstream(u, allowed); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	// 即使管理员误把 0.0.0.0/0 登记为允许网段，元数据地址仍被拒绝。
	if err := ValidateUpstream("169.254.169.254:80", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); err == nil {
		t.Error("metadata address accepted with 0.0.0.0/0")
	}
}

func TestValidateName(t *testing.T) {
	if ValidateName("site-example") != nil || ValidateName("Bad_Name") == nil || ValidateName("a") == nil {
		t.Fatal("name validation wrong")
	}
}
