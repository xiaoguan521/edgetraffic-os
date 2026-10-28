package render

import (
	"strings"
	"testing"
)

// Repack 必须与 Render 的清单算法一致：同样的文件得到同样的 Digest。
func TestRepackMatchesRender(t *testing.T) {
	in := sampleInput(t)
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for k, v := range b.Files {
		files[k] = v
	}
	rb := Repack(in.Revision, files)
	if rb.Manifest.Digest != b.Manifest.Digest {
		t.Fatalf("digest %s != %s", rb.Manifest.Digest, b.Manifest.Digest)
	}
	if string(rb.Files[ManifestName]) != string(b.Files[ManifestName]) {
		t.Fatal("manifest bytes differ")
	}
}

// 授权只读时 agent 只放行“仅证书文件变化”的版本（规格书 8.6：证书续期在任何授权状态下可用）。
func TestCertOnlyChange(t *testing.T) {
	base := map[string][]byte{
		"syapp.conf":            []byte("a"),
		"conf.d/site-shop.conf": []byte("b"),
		"certs/site-shop.crt":   []byte("c1"),
		"certs/site-shop.key":   []byte("k1"),
		ManifestName:            []byte("m1"),
	}
	clone := func(f func(m map[string][]byte)) map[string][]byte {
		m := map[string][]byte{}
		for k, v := range base {
			m[k] = v
		}
		f(m)
		return m
	}
	cases := []struct {
		name string
		next map[string][]byte
		want bool
	}{
		{"cert and key replaced", clone(func(m map[string][]byte) {
			m["certs/site-shop.crt"], m["certs/site-shop.key"], m[ManifestName] = []byte("c2"), []byte("k2"), []byte("m2")
		}), true},
		{"identical", clone(func(map[string][]byte) {}), false},
		{"config changed too", clone(func(m map[string][]byte) {
			m["certs/site-shop.crt"], m["conf.d/site-shop.conf"] = []byte("c2"), []byte("evil")
		}), false},
		{"new cert file added", clone(func(m map[string][]byte) { m["certs/site-blog.crt"] = []byte("x") }), false},
		{"cert file removed", clone(func(m map[string][]byte) { delete(m, "certs/site-shop.key") }), false},
		{"non-cert file under certs", clone(func(m map[string][]byte) { m["certs/site-shop.crt"], m["certs/x.conf"] = []byte("c2"), []byte("x") }), false},
	}
	for _, c := range cases {
		if got := CertOnlyChange(base, c.next); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// ACME HTTP-01：挑战片段只由校验过的 token / key authorization 生成，agent 只放行严格符合格式的片段。
func TestAcmeChallengeSnippet(t *testing.T) {
	in := sampleInput(t)
	tok := "abcDEF0123456789_-tokenXYZ"
	ka := tok + ".thumbprintABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"
	in.AcmeChallenges = []AcmeChallenge{{Token: tok, KeyAuth: ka}}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	snip := string(b.Files[AcmeSnippet])
	if !strings.Contains(snip, "/.well-known/acme-challenge/"+tok) || !strings.Contains(snip, `"`+ka+`"`) {
		t.Fatalf("snippet missing challenge:\n%s", snip)
	}
	if !ValidAcmeSnippet(b.Files[AcmeSnippet]) {
		t.Fatal("rendered snippet rejected by the validator")
	}
	for name, f := range b.Files {
		if strings.HasPrefix(name, "conf.d/site-") && !strings.Contains(string(f), "include  snippets/acme-challenge.conf;") {
			t.Fatalf("%s does not include the ACME snippet", name)
		}
	}
	empty := sampleInput(t)
	eb, _ := Render(empty)
	if !ValidAcmeSnippet(eb.Files[AcmeSnippet]) {
		t.Fatal("empty snippet rejected")
	}

	for _, bad := range []AcmeChallenge{
		{Token: "short", KeyAuth: "short.x"},
		{Token: tok + `";} location / { return 200 "x`, KeyAuth: ka},
		{Token: tok, KeyAuth: tok + `.x"; } server { listen 9 `},
		{Token: tok, KeyAuth: "other0123456789abcdef.thumbprintABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"},
	} {
		in := sampleInput(t)
		in.AcmeChallenges = []AcmeChallenge{bad}
		if _, err := Render(in); err == nil {
			t.Errorf("bad challenge accepted: %+v", bad)
		}
	}
	for _, bad := range []string{
		"location / { proxy_pass http://10.0.0.1; }\n",
		"location = /.well-known/acme-challenge/" + tok + " { default_type text/plain; return 200 \"" + ka + "\"; }\nlocation /x { }\n",
		"include /etc/passwd;\n",
	} {
		if ValidAcmeSnippet([]byte(bad)) {
			t.Errorf("validator accepted %q", bad)
		}
	}
}

// 证书续期类变更也可以同时更新 ACME 挑战片段，但片段内容必须合规。
func TestCertOnlyChangeWithAcmeSnippet(t *testing.T) {
	tok := "abcDEF0123456789_-tokenXYZ"
	good := []byte("# ACME HTTP-01 challenges (generated)\nlocation = /.well-known/acme-challenge/" + tok + " { default_type text/plain; return 200 \"" + tok + ".thumbprintABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab\"; }\n")
	base := map[string][]byte{"syapp.conf": []byte("a"), AcmeSnippet: []byte("# ACME HTTP-01 challenges (generated)\n"), ManifestName: []byte("m")}
	next := map[string][]byte{"syapp.conf": []byte("a"), AcmeSnippet: good, ManifestName: []byte("m2")}
	if !CertOnlyChange(base, next) {
		t.Fatal("valid challenge snippet change rejected")
	}
	next[AcmeSnippet] = []byte("location / { return 200 \"pwned\"; }\n")
	if CertOnlyChange(base, next) {
		t.Fatal("arbitrary config passed through the ACME snippet")
	}
}
