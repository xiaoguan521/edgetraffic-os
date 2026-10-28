package render

import (
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
