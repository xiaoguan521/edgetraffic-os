package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

// 正式签发的在线侧命令：生成签名请求、导入离线签名结果。都不接触私钥。

// cmdRequest 生成（或追加到）签名请求文件。
func cmdRequest(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	batch := fs.String("batch", "", "批次号，如 B0001")
	kid := fs.String("kid", "", "签名密钥编号（离线签名机上的正式密钥，如 etos-prod-2027）")
	reqPath := fs.String("request", "", "客户控制台导出的 activation-request.json")
	instID := fs.String("installation-id", "", "直接指定安装 ID（仅配合 -binding installation_only）")
	customer := fs.String("customer", "", "客户名称")
	customerID := fs.String("customer-id", "", "客户编号")
	edition := fs.String("edition", "professional", "trial | standard | professional | enterprise")
	nodes := fs.Int("nodes", 10, "节点数上限（0 表示不限）")
	sites := fs.Int("sites", 200, "站点数上限（0 表示不限）")
	days := fs.Int("days", 365, "有效天数")
	expires := fs.String("expires", "", "到期日 YYYY-MM-DD（覆盖 -days）")
	grace := fs.Int("grace", 15, "宽限天数")
	maint := fs.String("maintenance", "", "维保截止日 YYYY-MM-DD（默认同到期日）")
	binding := fs.String("binding", "installation", "installation | installation_only")
	licID := fs.String("id", "", "License 编号（默认自动生成）")
	createdBy := fs.String("by", os.Getenv("USER"), "导出人")
	outPath := fs.String("out", "", "签名请求文件；已存在时追加到其中（同一批次、同一密钥）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outPath == "" || *kid == "" {
		return errors.New("需要 -out 与 -kid")
	}
	if demoKID(*kid) {
		return errors.New("签名请求只用于正式密钥；演示 / CI 授权请用 issue")
	}
	var req license.ActivationRequest
	if *reqPath != "" {
		b, err := os.ReadFile(*reqPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &req); err != nil {
			return err
		}
	} else {
		if *binding != "installation_only" {
			return errors.New("-installation-id 只能配合 -binding installation_only（主机指纹来自激活申请文件）")
		}
		req.InstallationID = *instID
	}
	now := time.Now()
	o := license.IssueOptions{KID: *kid, LicenseID: *licID, CustomerID: *customerID, CustomerName: *customer,
		Edition: license.Edition(*edition), Nodes: *nodes, Sites: *sites, Days: *days, GraceDays: *grace,
		Binding: *binding, Request: req, ProductName: version.Product, Now: now}
	var err error
	if *expires != "" {
		if o.ExpiresAt, err = time.Parse("2006-01-02", *expires); err != nil {
			return err
		}
	}
	if *maint != "" {
		if o.Maintenance, err = time.Parse("2006-01-02", *maint); err != nil {
			return err
		}
	}
	p, err := license.BuildPayload(o)
	if err != nil {
		return err
	}
	sr, err := loadOrNewRequest(*outPath, *batch, *kid, *createdBy, now)
	if err != nil {
		return err
	}
	sr.Licenses = append(sr.Licenses, *p)
	if err := writeRequest(*outPath, sr, now); err != nil {
		return err
	}
	fmt.Fprintf(out, "已加入 %s：%s %s %s，节点 %d，到期 %s（批次共 %d 条）\n", *outPath, p.LicenseID, p.Customer.Name, p.Edition,
		p.Limits.Nodes, p.ExpiresAt.Format("2006-01-02"), len(sr.Licenses))
	return nil
}

// cmdCRLRequest 生成吊销列表的签名请求。CRL 是累积的：以 -base 指定的当前 CRL 为基础追加。
func cmdCRLRequest(args []string, out io.Writer, keys license.KeyRing) error {
	fs := flag.NewFlagSet("crl-request", flag.ContinueOnError)
	batch := fs.String("batch", "", "批次号")
	kid := fs.String("kid", "", "签名密钥编号")
	base := fs.String("base", "", "当前生效的吊销列表文件（新列表在它的基础上追加，序号自动加 1）")
	revoke := fs.String("revoke", "", "要吊销的 License 编号，逗号分隔")
	reason := fs.String("reason", "", "吊销原因")
	revokeKID := fs.String("revoke-kid", "", "要吊销的签名密钥编号，逗号分隔（私钥泄露时）")
	createdBy := fs.String("by", os.Getenv("USER"), "导出人")
	outPath := fs.String("out", "", "签名请求文件；已存在时把 CRL 写入其中")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outPath == "" || *kid == "" {
		return errors.New("需要 -out 与 -kid")
	}
	now := time.Now().UTC().Truncate(time.Second)
	c := &license.CRL{V: 1, KID: *kid, Serial: 1, IssuedAt: now}
	if *base != "" {
		b, err := os.ReadFile(*base)
		if err != nil {
			return err
		}
		prev, err := license.VerifyCRL(string(b), keys)
		if err != nil {
			return fmt.Errorf("当前吊销列表无法验证：%w", err)
		}
		c.Serial = prev.Serial + 1
		c.RevokedLicenses = append(c.RevokedLicenses, prev.RevokedLicenses...)
		c.RevokedKIDs = append(c.RevokedKIDs, prev.RevokedKIDs...)
	}
	for _, id := range splitCSV(*revoke) {
		if !c.LicenseRevoked(id) {
			c.RevokedLicenses = append(c.RevokedLicenses, license.RevokedLicense{LicenseID: id, RevokedAt: now, Reason: *reason})
		}
	}
	for _, k := range splitCSV(*revokeKID) {
		if !c.KIDRevoked(k) {
			c.RevokedKIDs = append(c.RevokedKIDs, k)
		}
	}
	sort.Strings(c.RevokedKIDs)
	sr, err := loadOrNewRequest(*outPath, *batch, *kid, *createdBy, now)
	if err != nil {
		return err
	}
	sr.CRL = c
	if err := writeRequest(*outPath, sr, now); err != nil {
		return err
	}
	fmt.Fprintf(out, "吊销列表序号 %d，共吊销 %d 个 License → %s\n", c.Serial, len(c.RevokedLicenses), *outPath)
	return nil
}

// cmdImport 核对离线签名结果并写出 .lic / .crl 文件。
func cmdImport(args []string, out io.Writer, keys license.KeyRing) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	reqPath := fs.String("request", "", "签名请求文件")
	signedPath := fs.String("signed", "", "离线签名机输出的 signed-<批次>.json")
	outDir := fs.String("out", ".", "输出目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*reqPath)
	if err != nil {
		return err
	}
	req, err := license.ParseSignRequest(raw, time.Now())
	if err != nil {
		return err
	}
	sb, err := os.ReadFile(*signedPath)
	if err != nil {
		return err
	}
	var s license.SignedBatch
	if err := json.Unmarshal(sb, &s); err != nil {
		return err
	}
	if err := license.VerifySignedBatch(req, raw, &s, keys); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for _, l := range s.Licenses {
		p := filepath.Join(*outDir, l.LicenseID+".lic")
		if err := os.WriteFile(p, []byte(l.Raw+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", p)
	}
	if s.CRL != "" {
		p := filepath.Join(*outDir, fmt.Sprintf("crl-%d.crl", req.CRL.Serial))
		if err := os.WriteFile(p, []byte(s.CRL+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", p)
	}
	fmt.Fprintf(out, "核对通过：批次 %s，%d 个 License\n", s.Batch, len(s.Licenses))
	return nil
}

func loadOrNewRequest(path, batch, kid, by string, now time.Time) (*license.SignRequest, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if batch == "" {
			return nil, errors.New("新建签名请求需要 -batch")
		}
		return &license.SignRequest{Format: license.SignRequestFormat, Batch: batch, KID: kid, CreatedAt: now.UTC(), CreatedBy: by}, nil
	}
	if err != nil {
		return nil, err
	}
	var sr license.SignRequest
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, err
	}
	if sr.KID != kid || (batch != "" && batch != sr.Batch) {
		return nil, fmt.Errorf("%s 属于批次 %s、密钥 %s", path, sr.Batch, sr.KID)
	}
	return &sr, nil
}

func writeRequest(path string, sr *license.SignRequest, now time.Time) error {
	if err := sr.Validate(now); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(sr, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func splitCSV(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
