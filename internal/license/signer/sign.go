package signer

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
)

// SignBatch 签名一个已通过校验的批次。kid 必须与私钥文件一致。
func SignBatch(req *license.SignRequest, reqRaw []byte, kid string, priv ed25519.PrivateKey, now time.Time) (*license.SignedBatch, error) {
	if req.KID != kid {
		return nil, fmt.Errorf("批次要求密钥 %s，当前私钥是 %s", req.KID, kid)
	}
	out := &license.SignedBatch{Format: license.SignedBatchFormat, Batch: req.Batch, KID: kid,
		RequestSHA256: license.Digest(reqRaw), SignedAt: now.UTC()}
	for i := range req.Licenses {
		raw, err := license.Sign(&req.Licenses[i], priv)
		if err != nil {
			return nil, err
		}
		out.Licenses = append(out.Licenses, license.SignedLicense{LicenseID: req.Licenses[i].LicenseID, Raw: raw})
	}
	if req.CRL != nil {
		raw, err := license.SignCRL(req.CRL, priv)
		if err != nil {
			return nil, err
		}
		out.CRL = raw
	}
	// 自检：用公钥把刚签好的结果完整核对一遍
	pub := priv.Public().(ed25519.PublicKey)
	if err := license.VerifySignedBatch(req, reqRaw, out, license.KeyRing{kid: pub}); err != nil {
		return nil, fmt.Errorf("签名自检失败：%w", err)
	}
	return out, nil
}

// Summary 生成签名前给签名员核对的清单。
func Summary(req *license.SignRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "批次 %s　密钥 %s　导出人 %s　导出时间 %s\n\n", req.Batch, req.KID, req.CreatedBy, req.CreatedAt.Local().Format("2006-01-02 15:04"))
	if len(req.Licenses) > 0 {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tLicense 编号\t客户\t版本\t节点\t站点\t生效\t到期\t绑定\t安装 ID")
		for i, p := range req.Licenses {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, p.LicenseID, p.Customer.Name, p.Edition,
				limit(p.Limits.Nodes), limit(p.Limits.Sites), p.NotBefore.Format("2006-01-02"), p.ExpiresAt.Format("2006-01-02"),
				p.Binding.Type, p.Binding.InstallationID)
		}
		_ = tw.Flush()
	}
	if c := req.CRL; c != nil {
		fmt.Fprintf(&b, "\n吊销列表：序号 %d，吊销 License %d 个", c.Serial, len(c.RevokedLicenses))
		if len(c.RevokedKIDs) > 0 {
			fmt.Fprintf(&b, "，吊销密钥 %s", strings.Join(c.RevokedKIDs, "、"))
		}
		b.WriteString("\n")
		for _, r := range c.RevokedLicenses {
			fmt.Fprintf(&b, "  - %s（%s）\n", r.LicenseID, r.Reason)
		}
	}
	return b.String()
}

func limit(n int) string {
	if n == 0 {
		return "不限"
	}
	return fmt.Sprint(n)
}

// LogEntry 是离线签名机本地签发日志的一行（JSON Lines），与后台审计相互印证。
type LogEntry struct {
	Time          time.Time `json:"time"`
	Batch         string    `json:"batch"`
	KID           string    `json:"kid"`
	RequestSHA256 string    `json:"request_sha256"`
	Licenses      []string  `json:"licenses,omitempty"`
	CRLSerial     uint64    `json:"crl_serial,omitempty"`
}

// AppendLog 追加一行签发日志（只追加，0600）。
func AppendLog(path string, req *license.SignRequest, out *license.SignedBatch) error {
	if path == "" {
		return errors.New("签发日志路径为空")
	}
	e := LogEntry{Time: out.SignedAt, Batch: out.Batch, KID: out.KID, RequestSHA256: out.RequestSHA256}
	for _, l := range out.Licenses {
		e.Licenses = append(e.Licenses, l.LicenseID)
	}
	if req.CRL != nil {
		e.CRLSerial = req.CRL.Serial
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Signed 检查签发日志中是否已经签过同一份请求（防止重复签名同一批次）。
func Signed(path, requestSHA string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		var e LogEntry
		if json.Unmarshal([]byte(ln), &e) == nil && e.RequestSHA256 == requestSHA {
			return true
		}
	}
	return false
}
