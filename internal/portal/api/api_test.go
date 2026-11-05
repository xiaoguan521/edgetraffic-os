package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/license/signer"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/service"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/portaltest"
)

type client struct {
	t   *testing.T
	url string
	hc  *http.Client
}

func newClient(t *testing.T, url string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, url: url, hc: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, csrf bool) (int, []byte) {
	c.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.url+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-Requested-With", "etos")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (c *client) ok(method, path string, body any) []byte {
	c.t.Helper()
	code, out := c.do(method, path, body, true)
	if code >= 300 {
		c.t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

func jsonID(t *testing.T, b []byte) float64 {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m["id"].(float64)
}

func TestHTTPFlow(t *testing.T) {
	st := portaltest.Open(t)
	srv := &Server{Svc: service.New(st), Store: st}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	root := newClient(t, ts.URL)

	// CSRF：没有自定义头的修改请求被拒绝
	if code, _ := root.do("POST", "/setup", map[string]string{"username": "root", "password": "correct horse 1"}, false); code != http.StatusForbidden {
		t.Fatalf("csrf: %d", code)
	}
	root.ok("POST", "/setup", map[string]string{"username": "root", "password": "correct horse 1"})
	if code, _ := newClient(t, ts.URL).do("GET", "/customers", nil, true); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", code)
	}
	// 空列表返回 []
	if out := root.ok("GET", "/batches", nil); string(out) != "[]" {
		t.Fatalf("empty list: %s", out)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root.ok("POST", "/keys", map[string]string{"kid": "etos-prod-2027", "public_key": base64.StdEncoding.EncodeToString(pub)})
	for _, u := range []map[string]any{
		{"username": "alice", "password": "sales pass 01", "roles": []string{"sales"}},
		{"username": "bob", "password": "business pass", "roles": []string{"business"}},
		{"username": "carol", "password": "signer pass 1", "roles": []string{"signer"}},
	} {
		root.ok("POST", "/users", u)
	}
	login := func(user, pass string) *client {
		c := newClient(t, ts.URL)
		c.ok("POST", "/auth/login", map[string]string{"username": user, "password": pass})
		return c
	}
	alice, bob, carol := login("alice", "sales pass 01"), login("bob", "business pass"), login("carol", "signer pass 1")

	cid := jsonID(t, alice.ok("POST", "/customers", map[string]string{"code": "C0001", "name": "某某公司"}))
	oid := jsonID(t, alice.ok("POST", "/orders", map[string]any{"order_no": "SO-1", "customer_id": cid, "edition": "standard", "nodes": 10, "days": 365, "grace_days": 15}))
	lid := jsonID(t, alice.ok("POST", "/licenses", map[string]any{"order_id": oid, "binding": "installation_only", "installation_id": "inst_abc"}))
	alice.ok("POST", "/licenses/"+itoa(lid)+"/submit", nil)
	// 销售不能审批
	if code, _ := alice.do("POST", "/licenses/"+itoa(lid)+"/approve", nil, true); code != http.StatusForbidden {
		t.Fatalf("sales approve: %d", code)
	}
	bob.ok("POST", "/licenses/"+itoa(lid)+"/approve", nil)
	bid := jsonID(t, carol.ok("POST", "/batches", map[string]any{"kid": "etos-prod-2027", "license_ids": []float64{lid}}))
	reqFile := carol.ok("GET", "/batches/"+itoa(bid)+"/request", nil)

	// 离线签名机（这里在测试中模拟）
	req, err := license.ParseSignRequest(reqFile, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.SignBatch(req, reqFile, req.KID, priv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(signed)
	// 销售不能导入签名结果
	if code, _ := alice.do("POST", "/batches/import", sb, true); code != http.StatusForbidden {
		t.Fatalf("sales import: %d", code)
	}
	carol.ok("POST", "/batches/import", sb)
	lic := alice.ok("GET", "/licenses/"+itoa(lid)+"/download", nil)
	raw, _ := license.SplitBundle(string(lic))
	if p, err := license.Verify(raw, license.KeyRing{"etos-prod-2027": pub}); err != nil || p.Binding.InstallationID != "inst_abc" {
		t.Fatalf("downloaded license: %v %v", p, err)
	}
	if out := root.ok("GET", "/audit/verify", nil); !strings.Contains(string(out), `"intact":true`) {
		t.Fatalf("audit: %s", out)
	}
	// 响应中不出现签名请求以外的敏感内容：用户列表不含口令哈希
	if out := root.ok("GET", "/users", nil); strings.Contains(string(out), "argon2") || strings.Contains(string(out), "password") {
		t.Fatalf("user list leaks password data: %s", out)
	}
	// 停用账号后，已登录的会话立即失效
	var users []map[string]any
	_ = json.Unmarshal(root.ok("GET", "/users", nil), &users)
	for _, u := range users {
		if u["username"] == "alice" {
			root.ok("PUT", "/users/"+itoa(u["id"].(float64)), map[string]any{"roles": []string{"sales"}, "disabled": true})
		}
	}
	if code, _ := alice.do("GET", "/customers", nil, true); code != http.StatusUnauthorized {
		t.Fatalf("disabled user still has a session: %d", code)
	}
}

func itoa(f float64) string { return strconv.Itoa(int(f)) }
