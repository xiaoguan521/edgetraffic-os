// Package service 是授权后台的业务逻辑（规格书 8.8 / 8.8.1）：
// 客户、订单、License 草稿 → 提交 → 审批 → 导出签名批次 → 离线签名 → 导入签发；吊销与吊销列表；签名公钥登记；审计。
//
// 授权后台只持有公钥：导入签名结果时用登记的公钥逐条核对（license.VerifySignedBatch），从不接触私钥。
package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gen/field"
	"gorm.io/gorm"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/query"
)

var (
	ErrForbidden    = errors.New("没有权限执行这个操作")
	ErrInvalidState = errors.New("当前状态不允许这个操作")
	ErrNotFound     = store.ErrNotFound
)

// Actor 是发起操作的用户。
type Actor struct {
	User  string
	Roles []string
	IP    string
}

// Has 报告用户是否拥有某个角色（admin 拥有全部权限）。
func (a Actor) Has(role string) bool {
	for _, r := range a.Roles {
		if r == role || r == store.RoleAdmin {
			return true
		}
	}
	return false
}

func (a Actor) need(roles ...string) error {
	for _, r := range roles {
		if a.Has(r) {
			return nil
		}
	}
	return ErrForbidden
}

// Service 是授权后台的业务服务。
type Service struct {
	Store *store.Store
	Now   func() time.Time

	audit auditLog
}

// New 创建服务。
func New(st *store.Store) *Service {
	s := &Service{Store: st, Now: time.Now}
	s.audit.q = query.Use(st.DB)
	return s
}

func (s *Service) q() *query.Query { return query.Use(s.Store.DB) }

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// ---------- 用户 ----------

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

var allRoles = map[string]bool{store.RoleAdmin: true, store.RoleSales: true, store.RoleBusiness: true, store.RoleSigner: true}

func normRoles(rs []string) (string, error) {
	set := map[string]bool{}
	for _, r := range rs {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !allRoles[r] {
			return "", fmt.Errorf("未知角色 %q", r)
		}
		set[r] = true
	}
	if len(set) == 0 {
		return "", errors.New("至少需要一个角色")
	}
	var out []string
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// Roles 拆分用户的角色字段。
func Roles(u *store.User) []string { return strings.Split(u.Roles, ",") }

// Initialized 报告是否已经创建过用户。
func (s *Service) Initialized(ctx context.Context) bool {
	n, _ := s.q().User.WithContext(ctx).Count()
	return n > 0
}

// Setup 创建第一个管理员（只能执行一次）。
func (s *Service) Setup(ctx context.Context, username, password, ip string) (*store.User, error) {
	if s.Initialized(ctx) {
		return nil, ErrInvalidState
	}
	return s.createUser(ctx, Actor{User: username, IP: ip}, username, "", password, []string{store.RoleAdmin}, "system.setup")
}

// Authenticate 校验用户名与口令。
func (s *Service) Authenticate(ctx context.Context, username, password string) (*store.User, error) {
	u := s.q().User
	row, err := u.WithContext(ctx).Where(u.Username.Eq(username)).First()
	if err != nil || row.Disabled || !crypto.CheckPassword(row.PasswordHash, password) {
		return nil, errors.New("用户名或口令错误")
	}
	_, _ = u.WithContext(ctx).Where(u.ID.Eq(row.ID)).Update(u.LastLoginAt, s.Now())
	return row, nil
}

// CreateUser 新建用户（管理员）。
func (s *Service) CreateUser(ctx context.Context, act Actor, username, display, password string, roles []string) (*store.User, error) {
	if err := act.need(store.RoleAdmin); err != nil {
		return nil, err
	}
	return s.createUser(ctx, act, username, display, password, roles, "user.create")
}

func (s *Service) createUser(ctx context.Context, act Actor, username, display, password string, roles []string, action string) (*store.User, error) {
	if !usernameRe.MatchString(username) {
		return nil, errors.New("用户名只能包含小写字母、数字和 . _ -，2–32 位，以字母开头")
	}
	if len([]rune(password)) < 10 {
		return nil, errors.New("口令至少 10 个字符")
	}
	rs, err := normRoles(roles)
	if err != nil {
		return nil, err
	}
	h, err := crypto.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &store.User{Username: username, DisplayName: display, PasswordHash: h, Roles: rs}
	err = s.q().Transaction(func(tx *query.Query) error {
		if err := tx.User.WithContext(ctx).Create(u); err != nil {
			return errors.New("用户已存在")
		}
		return s.audit.write(ctx, tx, act, action, "user", username, map[string]string{"roles": rs})
	})
	return u, err
}

// UpdateUser 修改角色、停用或重置口令（管理员）。管理员不能停用自己或去掉自己的管理员角色。
func (s *Service) UpdateUser(ctx context.Context, act Actor, id uint, roles []string, disabled bool, newPassword string) error {
	if err := act.need(store.RoleAdmin); err != nil {
		return err
	}
	rs, err := normRoles(roles)
	if err != nil {
		return err
	}
	u := s.q().User
	row, err := u.WithContext(ctx).Where(u.ID.Eq(id)).First()
	if err != nil {
		return notFound(err)
	}
	if row.Username == act.User && (disabled || !strings.Contains(","+rs+",", ","+store.RoleAdmin+",")) {
		return errors.New("不能停用自己，也不能去掉自己的管理员角色")
	}
	set := []field.AssignExpr{u.Roles.Value(rs), u.Disabled.Value(disabled)}
	if newPassword != "" {
		if len([]rune(newPassword)) < 10 {
			return errors.New("口令至少 10 个字符")
		}
		h, err := crypto.HashPassword(newPassword)
		if err != nil {
			return err
		}
		set = append(set, u.PasswordHash.Value(h))
	}
	return s.q().Transaction(func(tx *query.Query) error {
		if _, err := tx.User.WithContext(ctx).Where(tx.User.ID.Eq(id)).UpdateSimple(set...); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "user.update", "user", row.Username,
			map[string]any{"roles": rs, "disabled": disabled, "password_reset": newPassword != ""})
	})
}

// ListUsers 列出用户。
func (s *Service) ListUsers(ctx context.Context) ([]*store.User, error) {
	u := s.q().User
	return u.WithContext(ctx).Order(u.ID).Find()
}

// ---------- 签名公钥 ----------

// AddKey 登记一把签名公钥（管理员）。私钥永远只在离线签名机上。
func (s *Service) AddKey(ctx context.Context, act Actor, kid, pub, note string) (*store.SigningKey, error) {
	if err := act.need(store.RoleAdmin); err != nil {
		return nil, err
	}
	pk, err := license.ParsePublicKey(kid, pub)
	if err != nil {
		return nil, err
	}
	k := &store.SigningKey{KID: kid, PublicKey: strings.TrimSpace(pub), Fingerprint: license.KeyFingerprint(pk), Status: "active", Note: note, CreatedBy: act.User}
	err = s.q().Transaction(func(tx *query.Query) error {
		if err := tx.SigningKey.WithContext(ctx).Create(k); err != nil {
			return errors.New("这个密钥编号已经登记过")
		}
		return s.audit.write(ctx, tx, act, "key.add", "signing_key", kid, map[string]string{"fingerprint": k.Fingerprint})
	})
	return k, err
}

// SetKeyStatus 把密钥设为 active / retired / revoked（管理员）。吊销后不能恢复。
func (s *Service) SetKeyStatus(ctx context.Context, act Actor, id uint, status string) error {
	if err := act.need(store.RoleAdmin); err != nil {
		return err
	}
	if status != "active" && status != "retired" && status != "revoked" {
		return errors.New("状态只能是 active、retired 或 revoked")
	}
	k := s.q().SigningKey
	row, err := k.WithContext(ctx).Where(k.ID.Eq(id)).First()
	if err != nil {
		return notFound(err)
	}
	if row.Status == "revoked" {
		return errors.New("已吊销的密钥不能恢复")
	}
	return s.q().Transaction(func(tx *query.Query) error {
		if _, err := tx.SigningKey.WithContext(ctx).Where(tx.SigningKey.ID.Eq(id)).Update(tx.SigningKey.Status, status); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "key.status", "signing_key", row.KID, map[string]string{"from": row.Status, "to": status})
	})
}

// KeyView 是登记的公钥及其是否已内置在当前版本的控制台中。
type KeyView struct {
	*store.SigningKey
	Builtin bool `json:"builtin"` // 公钥已内置于本版本的 internal/license/keys.go
}

// ListKeys 列出登记的公钥。
func (s *Service) ListKeys(ctx context.Context) ([]KeyView, error) {
	k := s.q().SigningKey
	rows, err := k.WithContext(ctx).Order(k.ID).Find()
	if err != nil {
		return nil, err
	}
	builtin := license.BuiltinKeyRing()
	out := make([]KeyView, 0, len(rows))
	for _, r := range rows {
		pk, ok := builtin[r.KID]
		out = append(out, KeyView{SigningKey: r, Builtin: ok && pk.Equal(mustPub(r.PublicKey))})
	}
	return out, nil
}

func mustPub(b64 string) []byte {
	b, _ := decodeB64(b64)
	return b
}

// keyRing 返回用于核对签名结果的公钥（已吊销的密钥不包括在内）。
func (s *Service) keyRing(ctx context.Context, tx *query.Query) (license.KeyRing, error) {
	k := tx.SigningKey
	rows, err := k.WithContext(ctx).Where(k.Status.Neq("revoked")).Find()
	if err != nil {
		return nil, err
	}
	kr := license.KeyRing{}
	for _, r := range rows {
		pk, err := license.ParsePublicKey(r.KID, r.PublicKey)
		if err == nil {
			kr[r.KID] = pk
		}
	}
	return kr, nil
}

// ---------- 客户与订单 ----------

var codeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$`)

// CustomerInput 是新建 / 修改客户的参数。
type CustomerInput struct {
	Code, Name, Contact, Email, Phone, SalesOwner, Note string
}

func (in *CustomerInput) validate() error {
	if !codeRe.MatchString(in.Code) {
		return errors.New("客户编号只能包含字母、数字、- 和 _，2–32 位")
	}
	if strings.TrimSpace(in.Name) == "" || len([]rune(in.Name)) > 128 {
		return errors.New("客户名称不能为空，最多 128 个字符")
	}
	return nil
}

// CreateCustomer 新建客户（销售）。
func (s *Service) CreateCustomer(ctx context.Context, act Actor, in CustomerInput) (*store.Customer, error) {
	if err := act.need(store.RoleSales); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	c := &store.Customer{Code: in.Code, Name: strings.TrimSpace(in.Name), Contact: in.Contact, Email: in.Email, Phone: in.Phone,
		SalesOwner: in.SalesOwner, Note: in.Note, CreatedBy: act.User}
	err := s.q().Transaction(func(tx *query.Query) error {
		if err := tx.Customer.WithContext(ctx).Create(c); err != nil {
			return errors.New("客户编号已存在")
		}
		return s.audit.write(ctx, tx, act, "customer.create", "customer", c.Code, map[string]string{"name": c.Name})
	})
	return c, err
}

// UpdateCustomer 修改客户（销售）。客户编号不能修改。
func (s *Service) UpdateCustomer(ctx context.Context, act Actor, id uint, in CustomerInput) error {
	if err := act.need(store.RoleSales); err != nil {
		return err
	}
	c := s.q().Customer
	row, err := c.WithContext(ctx).Where(c.ID.Eq(id)).First()
	if err != nil {
		return notFound(err)
	}
	in.Code = row.Code
	if err := in.validate(); err != nil {
		return err
	}
	return s.q().Transaction(func(tx *query.Query) error {
		t := tx.Customer
		if _, err := t.WithContext(ctx).Where(t.ID.Eq(id)).UpdateSimple(t.Name.Value(strings.TrimSpace(in.Name)), t.Contact.Value(in.Contact),
			t.Email.Value(in.Email), t.Phone.Value(in.Phone), t.SalesOwner.Value(in.SalesOwner), t.Note.Value(in.Note)); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "customer.update", "customer", row.Code, map[string]string{"name": in.Name})
	})
}

// ListCustomers 列出客户，可按编号 / 名称搜索（不区分大小写的子串匹配；客户数量有限，在内存中过滤，
// 避免各数据库 LIKE 转义规则不同的问题）。
func (s *Service) ListCustomers(ctx context.Context, search string) ([]*store.Customer, error) {
	c := s.q().Customer
	rows, err := c.WithContext(ctx).Order(c.Code).Find()
	if err != nil {
		return nil, err
	}
	search = strings.ToLower(strings.TrimSpace(search))
	if search == "" {
		return rows, nil
	}
	out := rows[:0]
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Code), search) || strings.Contains(strings.ToLower(r.Name), search) {
			out = append(out, r)
		}
	}
	return out, nil
}

// OrderInput 是新建订单的参数。
type OrderInput struct {
	OrderNo    string
	CustomerID uint
	Edition    string
	Nodes      int
	Sites      int
	Days       int
	GraceDays  int
	Amount     string
	ContractNo string
	Note       string
}

// CreateOrder 新建订单（销售）。
func (s *Service) CreateOrder(ctx context.Context, act Actor, in OrderInput) (*store.Order, error) {
	if err := act.need(store.RoleSales); err != nil {
		return nil, err
	}
	switch {
	case !codeRe.MatchString(in.OrderNo):
		return nil, errors.New("订单号只能包含字母、数字、- 和 _，2–32 位")
	case license.EditionFeatures[license.Edition(in.Edition)] == nil:
		return nil, fmt.Errorf("未知版本 %q", in.Edition)
	case in.Nodes < 0 || in.Sites < 0 || in.Days < 1 || in.Days > 3650 || in.GraceDays < 0 || in.GraceDays > 90:
		return nil, errors.New("节点 / 站点数不能为负，有效天数 1–3650，宽限天数 0–90")
	}
	c := s.q().Customer
	cust, err := c.WithContext(ctx).Where(c.ID.Eq(in.CustomerID)).First()
	if err != nil {
		return nil, notFound(err)
	}
	o := &store.Order{OrderNo: in.OrderNo, CustomerID: cust.ID, Edition: in.Edition, Nodes: in.Nodes, Sites: in.Sites, Days: in.Days,
		GraceDays: in.GraceDays, Amount: in.Amount, ContractNo: in.ContractNo, Note: in.Note, CreatedBy: act.User}
	err = s.q().Transaction(func(tx *query.Query) error {
		if err := tx.Order.WithContext(ctx).Create(o); err != nil {
			return errors.New("订单号已存在")
		}
		return s.audit.write(ctx, tx, act, "order.create", "order", o.OrderNo,
			map[string]any{"customer": cust.Code, "edition": o.Edition, "nodes": o.Nodes, "days": o.Days})
	})
	return o, err
}

// ListOrders 列出订单（customerID 为 0 时列出全部）。
func (s *Service) ListOrders(ctx context.Context, customerID uint) ([]*store.Order, error) {
	o := s.q().Order
	sel := o.WithContext(ctx).Order(o.ID.Desc())
	if customerID != 0 {
		sel = sel.Where(o.CustomerID.Eq(customerID))
	}
	return sel.Find()
}

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

func decodeRawURL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
