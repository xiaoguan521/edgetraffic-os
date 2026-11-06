package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/license/signer"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/portaltest"
)

var (
	ctx      = context.Background()
	admin    = Actor{User: "root", Roles: []string{store.RoleAdmin}}
	sales    = Actor{User: "alice", Roles: []string{store.RoleSales}}
	business = Actor{User: "bob", Roles: []string{store.RoleBusiness}}
	signerA  = Actor{User: "carol", Roles: []string{store.RoleSigner}}
)

const actJSON = `{"installation_id":"inst_00ff","fp_hashes":["a1","b2","c3","d4","e5"],"product":"SyEdge","nodes":3}`

type env struct {
	s    *Service
	priv ed25519.PrivateKey
	kid  string
	cust *store.Customer
	ord  *store.Order
}

func setup(t *testing.T) *env {
	t.Helper()
	s := New(portaltest.Open(t))
	s.Now = func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.AddKey(ctx, admin, "etos-prod-2027", base64.StdEncoding.EncodeToString(pub), "测试"); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCustomer(ctx, sales, CustomerInput{Code: "C0001", Name: "某某公司", SalesOwner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.CreateOrder(ctx, sales, OrderInput{OrderNo: "SO-2026-001", CustomerID: c.ID, Edition: "professional", Nodes: 50, Sites: 2000, Days: 365, GraceDays: 15})
	if err != nil {
		t.Fatal(err)
	}
	return &env{s: s, priv: priv, kid: "etos-prod-2027", cust: c, ord: o}
}

// offlineSign 模拟离线签名机：解析导出的请求，用私钥签名。
func (e *env) offlineSign(t *testing.T, b *store.Batch, priv ed25519.PrivateKey) []byte {
	t.Helper()
	req, err := license.ParseSignRequest([]byte(b.Request), e.s.Now())
	if err != nil {
		t.Fatal(err)
	}
	out, err := signer.SignBatch(req, []byte(b.Request), req.KID, priv, e.s.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	return raw
}

func (e *env) approvedDraft(t *testing.T) *store.License {
	t.Helper()
	l, err := e.s.CreateDraft(ctx, sales, DraftInput{OrderID: e.ord.ID, ActivationRequest: actJSON})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Submit(ctx, sales, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Approve(ctx, business, l.ID); err != nil {
		t.Fatal(err)
	}
	return l
}

func status(t *testing.T, s *Service, id uint) string {
	t.Helper()
	v, err := s.GetLicense(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Status
}

// 完整流程：草稿 → 提交 → 审批 → 导出批次 → 离线签名 → 导入签发 → 下载；吊销 → 吊销列表 → 生效。
func TestFullIssuanceAndRevocation(t *testing.T) {
	e := setup(t)
	s := e.s
	l1 := e.approvedDraft(t)
	l2 := e.approvedDraft(t)
	if l1.LicenseID == l2.LicenseID || !strings.HasPrefix(l1.LicenseID, "LIC-20261102-") {
		t.Fatalf("license ids %s %s", l1.LicenseID, l2.LicenseID)
	}

	b, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l1.ID, l2.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if b.BatchNo != "B0001" || b.Licenses != 2 || status(t, s, l1.ID) != store.StatusBatched {
		t.Fatalf("batch %+v", b)
	}
	if strings.Contains(b.Request, "PRIVATE") {
		t.Fatal("request contains key material")
	}
	signed := e.offlineSign(t, b, e.priv)
	res, err := s.ImportSigned(ctx, signerA, signed)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issued) != 2 || status(t, s, l1.ID) != store.StatusIssued {
		t.Fatalf("import %+v", res)
	}
	// 导入第二次：批次已导入，拒绝
	if _, err := s.ImportSigned(ctx, signerA, signed); err == nil {
		t.Fatal("same signed batch imported twice")
	}

	name, content, err := s.Download(ctx, sales, l1.ID)
	if err != nil || name != l1.LicenseID+".lic" {
		t.Fatal(err)
	}
	lic, crls := license.SplitBundle(content)
	pub := e.priv.Public().(ed25519.PublicKey)
	p, err := license.Verify(lic, license.KeyRing{e.kid: pub})
	if err != nil || p.Customer.Name != "某某公司" || p.Limits.Nodes != 50 || p.KID != e.kid || len(p.Binding.FPHashes) != 5 || len(crls) != 0 {
		t.Fatalf("downloaded license: %+v %v", p, err)
	}

	// 吊销 l2：待发布 → 吊销列表批次 → 签名 → 导入后生效
	if err := s.Revoke(ctx, business, l2.ID, "合同终止"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, business, l2.ID, "again"); err == nil {
		t.Fatal("revoked twice")
	}
	v, _ := s.GetLicense(ctx, l2.ID)
	if v.Status != store.StatusIssued || !v.Revoking {
		t.Fatalf("pending revocation view %+v", v)
	}
	cb, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, IncludeCRL: true})
	if err != nil {
		t.Fatal(err)
	}
	if cb.CRLSerial != 1 {
		t.Fatalf("crl serial %d", cb.CRLSerial)
	}
	// 同一时间只能有一个带吊销列表的批次待签名
	if _, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, IncludeCRL: true}); err == nil {
		t.Fatal("second open CRL batch allowed")
	}
	if res, err := s.ImportSigned(ctx, signerA, e.offlineSign(t, cb, e.priv)); err != nil || res.CRLSerial != 1 {
		t.Fatalf("crl import %+v %v", res, err)
	}
	if status(t, s, l2.ID) != store.StatusRevoked {
		t.Fatal("license not revoked after CRL import")
	}
	if _, _, err := s.Download(ctx, sales, l2.ID); err == nil {
		t.Fatal("revoked license downloadable")
	}
	// 之后下载的授权文件附带最新吊销列表
	_, content, _ = s.Download(ctx, sales, l1.ID)
	if _, crls := license.SplitBundle(content); len(crls) != 1 {
		t.Fatal("download does not carry the latest CRL")
	} else if c, err := license.VerifyCRL(crls[0], license.KeyRing{e.kid: pub}); err != nil || !c.LicenseRevoked(l2.LicenseID) {
		t.Fatalf("crl %v %v", c, err)
	}

	// 第二份吊销列表是累积的
	l3 := e.approvedDraft(t)
	b3, _ := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l3.ID}})
	if _, err := s.ImportSigned(ctx, signerA, e.offlineSign(t, b3, e.priv)); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, business, l3.ID, "退款"); err != nil {
		t.Fatal(err)
	}
	cb2, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, IncludeCRL: true})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := license.ParseSignRequest([]byte(cb2.Request), s.Now())
	if req.CRL.Serial != 2 || !req.CRL.LicenseRevoked(l2.LicenseID) || !req.CRL.LicenseRevoked(l3.LicenseID) {
		t.Fatalf("CRL not cumulative: %+v", req.CRL)
	}

	if bad, err := s.VerifyAudit(ctx); err != nil || bad != 0 {
		t.Fatalf("audit chain: %d %v", bad, err)
	}
	d, err := s.Dashboard(ctx)
	if err != nil || d.Counts[store.StatusIssued] != 2 || d.Counts[store.StatusRevoked] != 1 || d.OpenBatches != 1 || !d.AuditIntact {
		t.Fatalf("dashboard %+v %v", d, err)
	}
}

// 角色与职责分离：销售不能审批，商务不能导出批次，审批人不能是提交人。
func TestRolesAndSeparationOfDuties(t *testing.T) {
	e := setup(t)
	s := e.s
	l, _ := s.CreateDraft(ctx, sales, DraftInput{OrderID: e.ord.ID, ActivationRequest: actJSON})
	if err := s.Approve(ctx, business, l.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("approved a draft that was never submitted: %v", err)
	}
	_ = s.Submit(ctx, sales, l.ID)
	if err := s.Approve(ctx, sales, l.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("sales approved: %v", err)
	}
	// 同时有销售与商务角色的人不能审批自己提交的
	both := Actor{User: "alice", Roles: []string{store.RoleSales, store.RoleBusiness}}
	if err := s.Approve(ctx, both, l.ID); err == nil || !strings.Contains(err.Error(), "审批人") {
		t.Fatalf("self approval: %v", err)
	}
	if _, err := s.ExportBatch(ctx, business, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("business exported: %v", err)
	}
	// 未审批的不能导出
	if _, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("exported an unapproved license: %v", err)
	}
	if err := s.Reject(ctx, business, l.ID, ""); err == nil {
		t.Fatal("reject without reason")
	}
	if err := s.Reject(ctx, business, l.ID, "节点数与合同不符"); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(ctx, sales, l.ID); err != nil { // 驳回后可修改并重新提交
		t.Fatal(err)
	}
	if _, err := s.AddKey(ctx, sales, "etos-prod-2028", "x", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("sales registered a key: %v", err)
	}
	if err := s.Revoke(ctx, sales, l.ID, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("sales revoked: %v", err)
	}
}

// 导入核对：被篡改的结果、另一把密钥签的、已吊销密钥签的，都拒绝；取消批次后 License 回到已审批。
func TestImportRejectsForgedAndCancel(t *testing.T) {
	e := setup(t)
	s := e.s
	l := e.approvedDraft(t)
	b, _ := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}})

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.ImportSigned(ctx, signerA, e.offlineSign(t, b, other)); err == nil {
		t.Fatal("batch signed by an unregistered key imported")
	}
	good := e.offlineSign(t, b, e.priv)
	tampered := bytes.Replace(good, []byte(`"raw":"ey`), []byte(`"raw":"ez`), 1)
	if _, err := s.ImportSigned(ctx, signerA, tampered); err == nil {
		t.Fatal("tampered batch imported")
	}
	if status(t, s, l.ID) != store.StatusBatched {
		t.Fatal("failed import changed state")
	}

	if err := s.CancelBatch(ctx, signerA, b.ID); err != nil {
		t.Fatal(err)
	}
	if status(t, s, l.ID) != store.StatusApproved {
		t.Fatal("license not returned to approved after cancel")
	}
	if _, err := s.ImportSigned(ctx, signerA, good); err == nil {
		t.Fatal("cancelled batch imported")
	}

	// 吊销密钥后，用它签的结果不再被接受；也不能再用它导出
	b2, _ := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}})
	keys, _ := s.ListKeys(ctx)
	if err := s.SetKeyStatus(ctx, admin, keys[0].ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSigned(ctx, signerA, e.offlineSign(t, b2, e.priv)); err == nil {
		t.Fatal("batch signed by a revoked key imported")
	}
	if err := s.SetKeyStatus(ctx, admin, keys[0].ID, "active"); err == nil {
		t.Fatal("revoked key re-activated")
	}
	_ = s.CancelBatch(ctx, signerA, b2.ID)
	if _, err := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}}); err == nil {
		t.Fatal("exported with a revoked key")
	}
}

// 续期：沿用客户、安装与指纹绑定，到期日从原到期日起算，并记录父 License；总览不再提醒已续期的。
func TestRenew(t *testing.T) {
	e := setup(t)
	s := e.s
	l := e.approvedDraft(t)
	b, _ := s.ExportBatch(ctx, signerA, ExportInput{KID: e.kid, LicenseIDs: []uint{l.ID}})
	if _, err := s.ImportSigned(ctx, signerA, e.offlineSign(t, b, e.priv)); err != nil {
		t.Fatal(err)
	}
	// 时间推进到原 License 到期前 30 天
	s.Now = func() time.Time { return time.Date(2027, 10, 3, 9, 0, 0, 0, time.UTC) }
	d, _ := s.Dashboard(ctx)
	if len(d.Expiring) != 1 || d.Expiring[0].LicenseID != l.LicenseID {
		t.Fatalf("expiring %+v", d.Expiring)
	}
	o2, _ := s.CreateOrder(ctx, sales, OrderInput{OrderNo: "SO-2027-001", CustomerID: e.cust.ID, Edition: "enterprise", Nodes: 80, Sites: 0, Days: 365, GraceDays: 15})
	r, err := s.Renew(ctx, sales, l.ID, o2.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetLicense(ctx, r.ID)
	orig, _ := s.GetLicense(ctx, l.ID)
	if v.ParentLicenseID != l.LicenseID || v.Payload.Edition != "enterprise" || v.Payload.Limits.Nodes != 80 ||
		!v.Payload.ExpiresAt.Equal(orig.Payload.ExpiresAt.AddDate(0, 0, 365)) || len(v.Payload.Binding.FPHashes) != 5 ||
		v.Payload.Binding.InstallationID != "inst_00ff" {
		t.Fatalf("renewal payload %+v", v.Payload)
	}
	if d, _ := s.Dashboard(ctx); len(d.Expiring) != 0 {
		t.Fatal("renewed license still listed as expiring")
	}
	// 其他客户的订单不能用来续期
	c2, _ := s.CreateCustomer(ctx, sales, CustomerInput{Code: "C0002", Name: "另一家"})
	o3, _ := s.CreateOrder(ctx, sales, OrderInput{OrderNo: "SO-X", CustomerID: c2.ID, Edition: "standard", Days: 30})
	if _, err := s.Renew(ctx, sales, l.ID, o3.ID); err == nil {
		t.Fatal("renewed with another customer's order")
	}
}

func TestUsersAndValidation(t *testing.T) {
	s := New(portaltest.Open(t))
	if _, err := s.Setup(ctx, "root", "short", ""); err == nil {
		t.Fatal("short password accepted")
	}
	u, err := s.Setup(ctx, "root", "correct horse 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup(ctx, "eve", "correct horse 2", ""); err == nil {
		t.Fatal("setup ran twice")
	}
	if _, err := s.Authenticate(ctx, "root", "wrong password"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := s.Authenticate(ctx, "root", "correct horse 1"); err != nil {
		t.Fatal(err)
	}
	a := Actor{User: "root", Roles: Roles(u)}
	b, err := s.CreateUser(ctx, a, "bob", "Bob", "business pass 1", []string{"business"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, a, "x", "", "long enough pw", []string{"business"}); err == nil {
		t.Fatal("bad username accepted")
	}
	if _, err := s.CreateUser(ctx, a, "eve", "", "long enough pw", []string{"root"}); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := s.UpdateUser(ctx, a, u.ID, []string{"sales"}, false, ""); err == nil {
		t.Fatal("admin removed own admin role")
	}
	if err := s.UpdateUser(ctx, a, b.ID, []string{"business"}, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "bob", "business pass 1"); err == nil {
		t.Fatal("disabled user logged in")
	}
	if _, err := s.AddKey(ctx, a, "etos-demo-2026", base64.StdEncoding.EncodeToString(make([]byte, 32)), ""); err == nil {
		t.Fatal("demo kid registered")
	}
	// LIKE 通配符不会被当成模式
	_, _ = s.CreateCustomer(ctx, Actor{User: "root", Roles: []string{"admin"}}, CustomerInput{Code: "C1", Name: "100%公司"})
	_, _ = s.CreateCustomer(ctx, Actor{User: "root", Roles: []string{"admin"}}, CustomerInput{Code: "C2", Name: "普通公司"})
	if cs, _ := s.ListCustomers(ctx, "%"); len(cs) != 1 {
		t.Fatalf("LIKE wildcard not escaped: %d", len(cs))
	}
}
