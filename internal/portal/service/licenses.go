package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gen/field"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

// ---------- License 草稿 ----------

// DraftInput 是新建草稿的参数：来自订单，加上客户控制台导出的激活申请（或容器部署时只给安装 ID）。
type DraftInput struct {
	OrderID           uint
	ActivationRequest string // activation-request.json 的内容
	InstallationID    string // 与 Binding=installation_only 一起使用
	Binding           string // installation（默认）| installation_only
	ExpiresAt         time.Time
	ParentLicenseID   string // 续期 / 扩容
}

// nextSeq 生成单调递增的编号（License 编号、批次号），在事务中调用。
func nextSeq(ctx context.Context, tx *query.Query, key string) (int, error) {
	kv := tx.SystemKV
	row, err := kv.WithContext(ctx).Where(kv.Key.Eq(key)).First()
	n := 0
	if err == nil {
		_, _ = fmt.Sscanf(row.Value, "%d", &n)
	}
	n++
	if err == nil {
		_, err = kv.WithContext(ctx).Where(kv.Key.Eq(key), kv.Value.Eq(row.Value)).Update(kv.Value, fmt.Sprint(n))
	} else {
		err = kv.WithContext(ctx).Create(&store.SystemKV{Key: key, Value: fmt.Sprint(n)})
	}
	return n, err
}

// CreateDraft 根据订单生成 License 草稿（销售）。
func (s *Service) CreateDraft(ctx context.Context, act Actor, in DraftInput) (*store.License, error) {
	if err := act.need(store.RoleSales); err != nil {
		return nil, err
	}
	q := s.q()
	ord, err := q.Order.WithContext(ctx).Where(q.Order.ID.Eq(in.OrderID)).First()
	if err != nil {
		return nil, notFound(err)
	}
	cust, err := q.Customer.WithContext(ctx).Where(q.Customer.ID.Eq(ord.CustomerID)).First()
	if err != nil {
		return nil, notFound(err)
	}
	var req license.ActivationRequest
	if strings.TrimSpace(in.ActivationRequest) != "" {
		if err := json.Unmarshal([]byte(in.ActivationRequest), &req); err != nil {
			return nil, errors.New("激活申请文件格式错误")
		}
	} else {
		if in.Binding != "installation_only" {
			return nil, errors.New("没有激活申请文件时只能使用“仅安装 ID”绑定（容器部署）")
		}
		req.InstallationID = strings.TrimSpace(in.InstallationID)
	}
	if in.Binding == "" {
		in.Binding = "installation"
	}
	now := s.Now()
	var lic *store.License
	err = q.Transaction(func(tx *query.Query) error {
		n, err := nextSeq(ctx, tx, "license_seq")
		if err != nil {
			return err
		}
		id := fmt.Sprintf("LIC-%s-%04d", now.UTC().Format("20060102"), n)
		p, err := license.BuildPayload(license.IssueOptions{LicenseID: id, CustomerID: cust.Code, CustomerName: cust.Name,
			Edition: license.Edition(ord.Edition), Nodes: ord.Nodes, Sites: ord.Sites, Days: ord.Days, ExpiresAt: in.ExpiresAt,
			GraceDays: ord.GraceDays, Binding: in.Binding, Request: req, ProductName: version.Product, Now: now})
		if err != nil {
			return err
		}
		pj, _ := json.Marshal(p)
		lic = &store.License{LicenseID: id, CustomerID: cust.ID, OrderID: ord.ID, ParentLicenseID: in.ParentLicenseID, Status: store.StatusDraft,
			Payload: string(pj), InstallationID: p.Binding.InstallationID, Binding: p.Binding.Type, ExpiresAt: p.ExpiresAt, CreatedBy: act.User}
		if err := tx.License.WithContext(ctx).Create(lic); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "license.draft", "license", id, map[string]any{"customer": cust.Code, "order": ord.OrderNo,
			"edition": p.Edition, "nodes": p.Limits.Nodes, "expires_at": p.ExpiresAt, "installation_id": p.Binding.InstallationID, "parent": in.ParentLicenseID})
	})
	return lic, err
}

// Renew 基于一张已签发的 License 生成续期 / 扩容草稿：沿用客户、安装与绑定；
// 新订单决定版本与数量，到期日从原到期日（或今天，取较晚者）起算。
func (s *Service) Renew(ctx context.Context, act Actor, licID uint, orderID uint) (*store.License, error) {
	if err := act.need(store.RoleSales); err != nil {
		return nil, err
	}
	l := s.q().License
	parent, err := l.WithContext(ctx).Where(l.ID.Eq(licID)).First()
	if err != nil {
		return nil, notFound(err)
	}
	if parent.Status != store.StatusIssued {
		return nil, fmt.Errorf("%w：只能续期已签发的 License", ErrInvalidState)
	}
	o := s.q().Order
	ord, err := o.WithContext(ctx).Where(o.ID.Eq(orderID)).First()
	if err != nil {
		return nil, notFound(err)
	}
	if ord.CustomerID != parent.CustomerID {
		return nil, errors.New("订单与原 License 不属于同一客户")
	}
	var p license.Payload
	if err := json.Unmarshal([]byte(parent.Payload), &p); err != nil {
		return nil, err
	}
	start := p.ExpiresAt
	if now := s.Now(); now.After(start) {
		start = now
	}
	req := license.ActivationRequest{InstallationID: p.Binding.InstallationID, FPHashes: p.Binding.FPHashes}
	rj, _ := json.Marshal(req)
	in := DraftInput{OrderID: ord.ID, Binding: p.Binding.Type, InstallationID: p.Binding.InstallationID,
		ExpiresAt: start.Add(time.Duration(ord.Days) * 24 * time.Hour), ParentLicenseID: parent.LicenseID}
	if p.Binding.Type == "installation" {
		in.ActivationRequest = string(rj)
	}
	return s.CreateDraft(ctx, act, in)
}

// transition 把 License 从允许的状态之一改为新状态（条件更新，防止并发重复操作）。
func (s *Service) transition(ctx context.Context, tx *query.Query, id uint, from []string, set ...field.AssignExpr) error {
	l := tx.License
	res, err := l.WithContext(ctx).Where(l.ID.Eq(id), l.Status.In(from...)).UpdateSimple(set...)
	if err != nil {
		return err
	}
	if res.RowsAffected != 1 {
		return ErrInvalidState
	}
	return nil
}

func (s *Service) getLicense(ctx context.Context, id uint) (*store.License, error) {
	l := s.q().License
	row, err := l.WithContext(ctx).Where(l.ID.Eq(id)).First()
	return row, notFound(err)
}

// Submit 提交草稿等待审批（销售）。被驳回的草稿可以重新提交。
func (s *Service) Submit(ctx context.Context, act Actor, id uint) error {
	if err := act.need(store.RoleSales); err != nil {
		return err
	}
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return err
	}
	return s.q().Transaction(func(tx *query.Query) error {
		l := tx.License
		if err := s.transition(ctx, tx, id, []string{store.StatusDraft, store.StatusRejected},
			l.Status.Value(store.StatusSubmitted), l.SubmittedBy.Value(act.User), l.RejectReason.Value("")); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "license.submit", "license", row.LicenseID, nil)
	})
}

// Approve 审批通过（商务）。审批人不能是提交人。
func (s *Service) Approve(ctx context.Context, act Actor, id uint) error {
	if err := act.need(store.RoleBusiness); err != nil {
		return err
	}
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return err
	}
	if row.SubmittedBy == act.User || row.CreatedBy == act.User {
		return errors.New("审批人不能是这张 License 的创建人或提交人")
	}
	return s.q().Transaction(func(tx *query.Query) error {
		l := tx.License
		if err := s.transition(ctx, tx, id, []string{store.StatusSubmitted}, l.Status.Value(store.StatusApproved), l.ApprovedBy.Value(act.User)); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "license.approve", "license", row.LicenseID, map[string]string{"submitted_by": row.SubmittedBy})
	})
}

// Reject 驳回（商务）。
func (s *Service) Reject(ctx context.Context, act Actor, id uint, reason string) error {
	if err := act.need(store.RoleBusiness); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("请填写驳回原因")
	}
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return err
	}
	return s.q().Transaction(func(tx *query.Query) error {
		l := tx.License
		if err := s.transition(ctx, tx, id, []string{store.StatusSubmitted}, l.Status.Value(store.StatusRejected), l.RejectReason.Value(reason)); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "license.reject", "license", row.LicenseID, map[string]string{"reason": reason})
	})
}

// DeleteDraft 删除尚未提交或被驳回的草稿（销售）。
func (s *Service) DeleteDraft(ctx context.Context, act Actor, id uint) error {
	if err := act.need(store.RoleSales); err != nil {
		return err
	}
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return err
	}
	return s.q().Transaction(func(tx *query.Query) error {
		l := tx.License
		res, err := l.WithContext(ctx).Where(l.ID.Eq(id), l.Status.In(store.StatusDraft, store.StatusRejected)).Delete()
		if err != nil {
			return err
		}
		if res.RowsAffected != 1 {
			return ErrInvalidState
		}
		return s.audit.write(ctx, tx, act, "license.delete_draft", "license", row.LicenseID, nil)
	})
}

// LicenseView 是 License 列表与详情的展示结构。
type LicenseView struct {
	*store.License
	CustomerCode string           `json:"customer_code"`
	CustomerName string           `json:"customer_name"`
	OrderNo      string           `json:"order_no"`
	Payload      *license.Payload `json:"payload"`
	Revoking     bool             `json:"revoking"` // 已申请吊销，等待吊销列表签名发布
}

// LicenseFilter 是列表筛选条件。
type LicenseFilter struct {
	Status     string
	CustomerID uint
}

// ListLicenses 列出 License。
func (s *Service) ListLicenses(ctx context.Context, f LicenseFilter) ([]LicenseView, error) {
	q := s.q()
	l := q.License
	sel := l.WithContext(ctx).Order(l.ID.Desc())
	if f.Status != "" {
		sel = sel.Where(l.Status.Eq(f.Status))
	}
	if f.CustomerID != 0 {
		sel = sel.Where(l.CustomerID.Eq(f.CustomerID))
	}
	rows, err := sel.Limit(1000).Find()
	if err != nil {
		return nil, err
	}
	return s.views(ctx, rows)
}

func (s *Service) views(ctx context.Context, rows []*store.License) ([]LicenseView, error) {
	q := s.q()
	custs, err := q.Customer.WithContext(ctx).Find()
	if err != nil {
		return nil, err
	}
	cmap := map[uint]*store.Customer{}
	for _, c := range custs {
		cmap[c.ID] = c
	}
	ords, err := q.Order.WithContext(ctx).Find()
	if err != nil {
		return nil, err
	}
	omap := map[uint]string{}
	for _, o := range ords {
		omap[o.ID] = o.OrderNo
	}
	pend, err := q.Revocation.WithContext(ctx).Where(q.Revocation.CRLSerial.Eq(0)).Find()
	if err != nil {
		return nil, err
	}
	pmap := map[string]bool{}
	for _, r := range pend {
		pmap[r.LicenseID] = true
	}
	out := make([]LicenseView, 0, len(rows))
	for _, r := range rows {
		v := LicenseView{License: r, OrderNo: omap[r.OrderID], Revoking: pmap[r.LicenseID]}
		if c := cmap[r.CustomerID]; c != nil {
			v.CustomerCode, v.CustomerName = c.Code, c.Name
		}
		var p license.Payload
		if json.Unmarshal([]byte(r.Payload), &p) == nil {
			v.Payload = &p
		}
		out = append(out, v)
	}
	return out, nil
}

// GetLicense 返回一张 License 的详情。
func (s *Service) GetLicense(ctx context.Context, id uint) (*LicenseView, error) {
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return nil, err
	}
	vs, err := s.views(ctx, []*store.License{row})
	if err != nil {
		return nil, err
	}
	return &vs[0], nil
}

// Download 返回发给客户的授权文件：License 加上当前最新的吊销列表（每行一项）。
func (s *Service) Download(ctx context.Context, act Actor, id uint) (name string, content string, err error) {
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return "", "", err
	}
	if row.Status != store.StatusIssued || row.Raw == "" {
		return "", "", fmt.Errorf("%w：只有已签发的 License 可以下载", ErrInvalidState)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# EdgeTraffic OS License %s\n%s\n", row.LicenseID, row.Raw)
	if crl, err := s.LatestCRL(ctx); err == nil && crl != nil {
		fmt.Fprintf(&b, "# 吊销列表（序号 %d）\n%s\n", crl.Serial, crl.Raw)
	}
	_ = s.q().Transaction(func(tx *query.Query) error {
		return s.audit.write(ctx, tx, act, "license.download", "license", row.LicenseID, nil)
	})
	return row.LicenseID + ".lic", b.String(), nil
}

// ---------- 吊销 ----------

// Revoke 申请吊销一张已签发的 License（商务）。吊销在下一份吊销列表签名发布后生效。
func (s *Service) Revoke(ctx context.Context, act Actor, id uint, reason string) error {
	if err := act.need(store.RoleBusiness); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("请填写吊销原因")
	}
	row, err := s.getLicense(ctx, id)
	if err != nil {
		return err
	}
	if row.Status != store.StatusIssued {
		return fmt.Errorf("%w：只能吊销已签发的 License", ErrInvalidState)
	}
	return s.q().Transaction(func(tx *query.Query) error {
		if err := tx.Revocation.WithContext(ctx).Create(&store.Revocation{LicenseID: row.LicenseID, Reason: reason, CreatedBy: act.User}); err != nil {
			return errors.New("这张 License 已经申请过吊销")
		}
		return s.audit.write(ctx, tx, act, "license.revoke", "license", row.LicenseID, map[string]string{"reason": reason})
	})
}

// ListRevocations 列出吊销记录。
func (s *Service) ListRevocations(ctx context.Context) ([]*store.Revocation, error) {
	r := s.q().Revocation
	return r.WithContext(ctx).Order(r.ID.Desc()).Find()
}

// LatestCRL 返回最新发布的吊销列表（没有时返回 nil）。
func (s *Service) LatestCRL(ctx context.Context) (*store.CRLRecord, error) {
	c := s.q().CRLRecord
	rows, err := c.WithContext(ctx).Order(c.Serial.Desc()).Limit(1).Find()
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// ListCRLs 列出已发布的吊销列表。
func (s *Service) ListCRLs(ctx context.Context) ([]*store.CRLRecord, error) {
	c := s.q().CRLRecord
	return c.WithContext(ctx).Order(c.Serial.Desc()).Find()
}

// ---------- 签名批次 ----------

// ExportInput 是导出签名批次的参数。
type ExportInput struct {
	KID        string
	LicenseIDs []uint // 已审批的 License
	IncludeCRL bool   // 把待发布的吊销做成新的吊销列表
}

// ExportBatch 把已审批的 License（以及待发布的吊销）导出为签名请求（签名员）。
func (s *Service) ExportBatch(ctx context.Context, act Actor, in ExportInput) (*store.Batch, error) {
	if err := act.need(store.RoleSigner); err != nil {
		return nil, err
	}
	if len(in.LicenseIDs) == 0 && !in.IncludeCRL {
		return nil, errors.New("请选择要签名的 License 或吊销列表")
	}
	now := s.Now().UTC().Truncate(time.Second)
	var batch *store.Batch
	err := s.q().Transaction(func(tx *query.Query) error {
		k := tx.SigningKey
		key, err := k.WithContext(ctx).Where(k.KID.Eq(in.KID)).First()
		if err != nil || key.Status != "active" {
			return fmt.Errorf("密钥 %s 未登记或不是启用状态", in.KID)
		}
		n, err := nextSeq(ctx, tx, "batch_seq")
		if err != nil {
			return err
		}
		batch = &store.Batch{BatchNo: fmt.Sprintf("B%04d", n), KID: in.KID, Status: "exported", ExportedBy: act.User, ExportedAt: now}
		req := &license.SignRequest{Format: license.SignRequestFormat, Batch: batch.BatchNo, KID: in.KID, CreatedAt: now, CreatedBy: act.User}
		l := tx.License
		rows, err := l.WithContext(ctx).Where(l.ID.In(in.LicenseIDs...)).Order(l.ID).Find()
		if err != nil {
			return err
		}
		if len(rows) != len(in.LicenseIDs) {
			return ErrNotFound
		}
		for _, r := range rows {
			if r.Status != store.StatusApproved {
				return fmt.Errorf("%w：%s 不是已审批状态", ErrInvalidState, r.LicenseID)
			}
			var p license.Payload
			if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
				return err
			}
			p.KID = in.KID
			req.Licenses = append(req.Licenses, p)
		}
		if in.IncludeCRL {
			c, err := s.nextCRL(ctx, tx, in.KID, now)
			if err != nil {
				return err
			}
			req.CRL = c
			batch.CRLSerial = c.Serial
		}
		if err := req.Validate(now); err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(req, "", "  ")
		raw = append(raw, '\n')
		batch.Request, batch.RequestSHA256, batch.Licenses = string(raw), license.Digest(raw), len(req.Licenses)
		if err := tx.Batch.WithContext(ctx).Create(batch); err != nil {
			return err
		}
		for _, r := range rows {
			if err := s.transition(ctx, tx, r.ID, []string{store.StatusApproved}, l.Status.Value(store.StatusBatched), l.BatchID.Value(batch.ID)); err != nil {
				return err
			}
		}
		return s.audit.write(ctx, tx, act, "batch.export", "batch", batch.BatchNo, map[string]any{"kid": in.KID, "licenses": batch.Licenses,
			"crl_serial": batch.CRLSerial, "request_sha256": batch.RequestSHA256})
	})
	return batch, err
}

// nextCRL 生成下一份（累积的）吊销列表：上一份已发布列表的全部项 + 待发布的吊销，序号加 1。
// 同一时间只允许一个包含吊销列表的批次处于待签名状态，避免序号冲突。
func (s *Service) nextCRL(ctx context.Context, tx *query.Query, kid string, now time.Time) (*license.CRL, error) {
	b := tx.Batch
	open, err := b.WithContext(ctx).Where(b.Status.Eq("exported"), b.CRLSerial.Gt(0)).Count()
	if err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, errors.New("已有一个包含吊销列表的批次在等待签名，请先导入或取消它")
	}
	r := tx.Revocation
	pend, err := r.WithContext(ctx).Where(r.CRLSerial.Eq(0)).Order(r.ID).Find()
	if err != nil {
		return nil, err
	}
	if len(pend) == 0 {
		return nil, errors.New("没有待发布的吊销")
	}
	c := &license.CRL{V: 1, KID: kid, Serial: 1, IssuedAt: now}
	cr := tx.CRLRecord
	last, err := cr.WithContext(ctx).Order(cr.Serial.Desc()).Limit(1).Find()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	if len(last) == 1 {
		prev, err := parseCRLUnverified(last[0].Raw)
		if err != nil {
			return nil, err
		}
		c.Serial = prev.Serial + 1
		c.RevokedLicenses = append(c.RevokedLicenses, prev.RevokedLicenses...)
		c.RevokedKIDs = append(c.RevokedKIDs, prev.RevokedKIDs...)
		for _, x := range prev.RevokedLicenses {
			seen[x.LicenseID] = true
		}
	}
	for _, p := range pend {
		if !seen[p.LicenseID] {
			c.RevokedLicenses = append(c.RevokedLicenses, license.RevokedLicense{LicenseID: p.LicenseID, RevokedAt: p.CreatedAt.UTC().Truncate(time.Second), Reason: p.Reason})
		}
	}
	// 已吊销的签名密钥也写入吊销列表
	k := tx.SigningKey
	revoked, err := k.WithContext(ctx).Where(k.Status.Eq("revoked")).Find()
	if err != nil {
		return nil, err
	}
	for _, rk := range revoked {
		if !c.KIDRevoked(rk.KID) && rk.KID != kid {
			c.RevokedKIDs = append(c.RevokedKIDs, rk.KID)
		}
	}
	sort.Strings(c.RevokedKIDs)
	return c, nil
}

// parseCRLUnverified 读取已发布（导入时已验签）的吊销列表内容。
func parseCRLUnverified(raw string) (*license.CRL, error) {
	// 导入时已经用登记的公钥验证过；这里只取内容作为下一份列表的基础
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "etos-crl1."), ".")
	if len(parts) != 2 {
		return nil, errors.New("已发布的吊销列表格式错误")
	}
	body, err := decodeRawURL(parts[0])
	if err != nil {
		return nil, err
	}
	var c license.CRL
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ImportResult 描述一次导入。
type ImportResult struct {
	Batch     *store.Batch `json:"batch"`
	Issued    []string     `json:"issued"`
	CRLSerial uint64       `json:"crl_serial"`
}

// ImportSigned 导入离线签名机输出的签名结果（签名员）：用登记的公钥逐条核对，通过后签发 License、发布吊销列表。
func (s *Service) ImportSigned(ctx context.Context, act Actor, signedRaw []byte) (*ImportResult, error) {
	if err := act.need(store.RoleSigner); err != nil {
		return nil, err
	}
	var sb license.SignedBatch
	if err := json.Unmarshal(signedRaw, &sb); err != nil {
		return nil, errors.New("签名结果文件格式错误")
	}
	res := &ImportResult{}
	now := s.Now()
	err := s.q().Transaction(func(tx *query.Query) error {
		b := tx.Batch
		batch, err := b.WithContext(ctx).Where(b.BatchNo.Eq(sb.Batch)).First()
		if err != nil {
			return fmt.Errorf("找不到批次 %s", sb.Batch)
		}
		if batch.Status != "exported" {
			return fmt.Errorf("%w：批次 %s 的状态是 %s", ErrInvalidState, batch.BatchNo, batch.Status)
		}
		req, err := license.ParseSignRequest([]byte(batch.Request), now)
		if err != nil {
			return err
		}
		keys, err := s.keyRing(ctx, tx)
		if err != nil {
			return err
		}
		if err := license.VerifySignedBatch(req, []byte(batch.Request), &sb, keys); err != nil {
			return fmt.Errorf("签名结果核对失败：%w", err)
		}
		l := tx.License
		for _, sl := range sb.Licenses {
			if err := s.issue(ctx, tx, batch.ID, sl, now); err != nil {
				return err
			}
		}
		for _, sl := range sb.Licenses {
			res.Issued = append(res.Issued, sl.LicenseID)
		}
		if sb.CRL != "" {
			c, err := license.VerifyCRL(sb.CRL, keys)
			if err != nil {
				return err
			}
			if err := tx.CRLRecord.WithContext(ctx).Create(&store.CRLRecord{Serial: c.Serial, KID: c.KID, Raw: sb.CRL,
				Revoked: len(c.RevokedLicenses), BatchID: batch.ID, PublishedAt: now}); err != nil {
				return errors.New("这个序号的吊销列表已经发布过")
			}
			ids := make([]string, 0, len(c.RevokedLicenses))
			for _, x := range c.RevokedLicenses {
				ids = append(ids, x.LicenseID)
			}
			r := tx.Revocation
			if _, err := r.WithContext(ctx).Where(r.CRLSerial.Eq(0), r.LicenseID.In(ids...)).Update(r.CRLSerial, c.Serial); err != nil {
				return err
			}
			if len(ids) > 0 {
				if _, err := l.WithContext(ctx).Where(l.LicenseID.In(ids...), l.Status.Eq(store.StatusIssued)).
					UpdateSimple(l.Status.Value(store.StatusRevoked), l.RevokedAt.Value(now)); err != nil {
					return err
				}
			}
			res.CRLSerial = c.Serial
		}
		if _, err := b.WithContext(ctx).Where(b.ID.Eq(batch.ID), b.Status.Eq("exported")).UpdateSimple(b.Status.Value("imported"),
			b.Signed.Value(string(signedRaw)), b.ImportedBy.Value(act.User), b.ImportedAt.Value(now)); err != nil {
			return err
		}
		batch.Status = "imported"
		res.Batch = batch
		return s.audit.write(ctx, tx, act, "batch.import", "batch", batch.BatchNo, map[string]any{"issued": len(sb.Licenses),
			"crl_serial": res.CRLSerial, "request_sha256": sb.RequestSHA256})
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) issue(ctx context.Context, tx *query.Query, batchID uint, sl license.SignedLicense, now time.Time) error {
	l := tx.License
	r, err := l.WithContext(ctx).Where(l.LicenseID.Eq(sl.LicenseID), l.BatchID.Eq(batchID), l.Status.Eq(store.StatusBatched)).
		UpdateSimple(l.Status.Value(store.StatusIssued), l.Raw.Value(sl.Raw), l.IssuedAt.Value(now))
	if err != nil {
		return err
	}
	if r.RowsAffected != 1 {
		return fmt.Errorf("%w：%s 不在这个批次中或状态已改变", ErrInvalidState, sl.LicenseID)
	}
	return nil
}

// CancelBatch 取消一个尚未导入的批次（签名员），其中的 License 回到已审批状态。
func (s *Service) CancelBatch(ctx context.Context, act Actor, id uint) error {
	if err := act.need(store.RoleSigner); err != nil {
		return err
	}
	return s.q().Transaction(func(tx *query.Query) error {
		b := tx.Batch
		batch, err := b.WithContext(ctx).Where(b.ID.Eq(id)).First()
		if err != nil {
			return notFound(err)
		}
		res, err := b.WithContext(ctx).Where(b.ID.Eq(id), b.Status.Eq("exported")).Update(b.Status, "cancelled")
		if err != nil {
			return err
		}
		if res.RowsAffected != 1 {
			return ErrInvalidState
		}
		l := tx.License
		if _, err := l.WithContext(ctx).Where(l.BatchID.Eq(id), l.Status.Eq(store.StatusBatched)).
			UpdateSimple(l.Status.Value(store.StatusApproved), l.BatchID.Value(0)); err != nil {
			return err
		}
		return s.audit.write(ctx, tx, act, "batch.cancel", "batch", batch.BatchNo, nil)
	})
}

// ListBatches 列出签名批次。
func (s *Service) ListBatches(ctx context.Context) ([]*store.Batch, error) {
	b := s.q().Batch
	return b.WithContext(ctx).Order(b.ID.Desc()).Find()
}

// BatchRequest 返回批次的签名请求文件（用于拷到离线签名机）。
func (s *Service) BatchRequest(ctx context.Context, act Actor, id uint) (*store.Batch, error) {
	if err := act.need(store.RoleSigner); err != nil {
		return nil, err
	}
	b := s.q().Batch
	row, err := b.WithContext(ctx).Where(b.ID.Eq(id)).First()
	return row, notFound(err)
}

// ---------- 总览 ----------

// Dashboard 是总览页数据。
type Dashboard struct {
	Counts      map[string]int64 `json:"counts"`
	Pending     int64            `json:"pending_revocations"`
	OpenBatches int64            `json:"open_batches"`
	Expiring    []LicenseView    `json:"expiring"` // 60 天内到期、尚未续期的已签发 License（供销售跟进）
	ActiveKeys  int64            `json:"active_keys"`
	LatestCRL   uint64           `json:"latest_crl"`
	AuditIntact bool             `json:"audit_intact"`
}

// Dashboard 汇总授权后台的状态。
func (s *Service) Dashboard(ctx context.Context) (*Dashboard, error) {
	q := s.q()
	d := &Dashboard{Counts: map[string]int64{}}
	for _, st := range []string{store.StatusDraft, store.StatusSubmitted, store.StatusApproved, store.StatusRejected, store.StatusBatched, store.StatusIssued, store.StatusRevoked} {
		n, err := q.License.WithContext(ctx).Where(q.License.Status.Eq(st)).Count()
		if err != nil {
			return nil, err
		}
		d.Counts[st] = n
	}
	d.Pending, _ = q.Revocation.WithContext(ctx).Where(q.Revocation.CRLSerial.Eq(0)).Count()
	d.OpenBatches, _ = q.Batch.WithContext(ctx).Where(q.Batch.Status.Eq("exported")).Count()
	d.ActiveKeys, _ = q.SigningKey.WithContext(ctx).Where(q.SigningKey.Status.Eq("active")).Count()
	if c, err := s.LatestCRL(ctx); err == nil && c != nil {
		d.LatestCRL = c.Serial
	}
	l := q.License
	rows, err := l.WithContext(ctx).Where(l.Status.Eq(store.StatusIssued), l.ExpiresAt.Lt(s.Now().AddDate(0, 0, 60))).Order(l.ExpiresAt).Limit(50).Find()
	if err != nil {
		return nil, err
	}
	// 已有续期 License（以它为父）的不再提醒
	renewed := map[string]bool{}
	kids, err := l.WithContext(ctx).Where(l.ParentLicenseID.Neq(""), l.Status.Neq(store.StatusRejected)).Find()
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		renewed[k.ParentLicenseID] = true
	}
	var keep []*store.License
	for _, r := range rows {
		if !renewed[r.LicenseID] {
			keep = append(keep, r)
		}
	}
	if d.Expiring, err = s.views(ctx, keep); err != nil {
		return nil, err
	}
	bad, err := s.VerifyAudit(ctx)
	d.AuditIntact = err == nil && bad == 0
	return d, nil
}

// ListAudit 返回最近的审计记录。
func (s *Service) ListAudit(ctx context.Context, limit int) ([]*store.AuditLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	a := s.q().AuditLog
	return a.WithContext(ctx).Order(a.ID.Desc()).Limit(limit).Find()
}
