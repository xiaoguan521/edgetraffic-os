// syapp-license-server：厂商授权工具（规格书 8.8）。部署在厂商内网，不交付给客户。
//
// MVP 提供命令行签发；授权后台 Web 界面在 P4 实现。
//
//	syapp-license-server keygen -kid etos-prod-2027 -out keys/
//	syapp-license-server issue  -key keys/etos-demo-2026.key -kid etos-demo-2026 \
//	    -request activation-request.json -customer "某某公司" -edition professional \
//	    -nodes 50 -sites 2000 -days 365 [-binding installation_only] -out customer.lic
//	syapp-license-server inspect customer.lic
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

var editionFeatures = map[license.Edition][]license.Feature{
	license.EditionTrial:        {license.FeatureCore},
	license.EditionStandard:     {license.FeatureCore, license.FeatureAPIToken},
	license.EditionProfessional: {license.FeatureCore, license.FeatureAPIToken, license.FeatureBatchRelease, license.FeatureReleaseApproval, license.FeatureSSO, license.FeatureAuditExport, license.FeatureAlertIM, license.FeatureStream, license.FeatureHA},
	license.EditionEnterprise:   {license.FeatureCore, license.FeatureAPIToken, license.FeatureBatchRelease, license.FeatureReleaseApproval, license.FeatureSSO, license.FeatureAuditExport, license.FeatureAlertIM, license.FeatureStream, license.FeatureHA, license.FeatureCanary, license.FeatureGMTLS},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2:])
	case "issue":
		issue(os.Args[2:])
	case "inspect":
		inspect(os.Args[2:])
	case "version", "-version":
		fmt.Println(version.String("license-server"))
	default:
		usage()
	}
}

func keygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	kid := fs.String("kid", "", "密钥编号，例如 etos-prod-2027")
	out := fs.String("out", ".", "输出目录")
	_ = fs.Parse(args)
	if *kid == "" {
		die("-kid is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	check(err)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	check(os.MkdirAll(*out, 0o700))
	keyPath := filepath.Join(*out, *kid+".key")
	check(os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	fmt.Printf("private key: %s（妥善离线保管，切勿提交到代码仓库）\n", keyPath)
	fmt.Printf("public key for internal/license/keys.go:\n    %q: %q,\n", *kid, base64.StdEncoding.EncodeToString(pub))
}

func issue(args []string) {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	keyPath := fs.String("key", "", "签名私钥（PEM）")
	kid := fs.String("kid", "", "密钥编号（须内置于控制台）")
	reqPath := fs.String("request", "", "控制台导出的激活申请 activation-request.json")
	customer := fs.String("customer", "", "客户名称")
	customerID := fs.String("customer-id", "", "客户编号")
	edition := fs.String("edition", "professional", "trial | standard | professional | enterprise")
	nodes := fs.Int("nodes", 10, "节点数上限（0 表示不限）")
	sites := fs.Int("sites", 200, "站点数上限（0 表示不限）")
	days := fs.Int("days", 365, "有效天数")
	expires := fs.String("expires", "", "到期日 YYYY-MM-DD（覆盖 -days，可用于签发已过期的演示授权）")
	grace := fs.Int("grace", 15, "宽限天数")
	binding := fs.String("binding", "installation", "installation（安装 ID + 主机指纹）| installation_only（容器部署）")
	licID := fs.String("id", "", "License 编号（默认自动生成）")
	out := fs.String("out", "", "输出文件（默认打印到标准输出）")
	_ = fs.Parse(args)
	if *keyPath == "" || *kid == "" || *reqPath == "" || *customer == "" {
		die("-key, -kid, -request and -customer are required")
	}
	feats, ok := editionFeatures[license.Edition(*edition)]
	if !ok {
		die("unknown edition " + *edition)
	}
	priv := loadKey(*keyPath)
	var req struct {
		InstallationID string   `json:"installation_id"`
		FPHashes       []string `json:"fp_hashes"`
	}
	b, err := os.ReadFile(*reqPath)
	check(err)
	check(json.Unmarshal(b, &req))
	if !strings.HasPrefix(req.InstallationID, "inst_") {
		die("activation request has no installation_id")
	}
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(time.Duration(*days) * 24 * time.Hour)
	if *expires != "" {
		t, err := time.Parse("2006-01-02", *expires)
		check(err)
		exp = t.UTC()
	}
	if *licID == "" {
		*licID = fmt.Sprintf("LIC-%s-%04d", now.Format("20060102"), now.Unix()%10000)
	}
	notBefore := now
	if exp.Before(now) {
		notBefore = exp.Add(-30 * 24 * time.Hour)
	}
	p := &license.Payload{
		V: 1, KID: *kid, LicenseID: *licID,
		Customer: license.Customer{ID: *customerID, Name: *customer},
		Edition:  license.Edition(*edition), Features: feats,
		Limits:   license.Limits{Nodes: *nodes, Sites: *sites, AuditDays: 365},
		IssuedAt: now, NotBefore: notBefore, ExpiresAt: exp, MaintenanceUntil: exp, GraceDays: *grace,
		Binding: license.Binding{Type: *binding, InstallationID: req.InstallationID},
		Product: license.Product{Name: version.Product, MinVersion: "0.0.0", MaxVersion: "1.*"},
	}
	if *binding == "installation" {
		p.Binding.FPHashes = req.FPHashes
	}
	raw, err := license.Sign(p, priv)
	check(err)
	if *out == "" {
		fmt.Println(raw)
		return
	}
	check(os.WriteFile(*out, []byte(raw+"\n"), 0o644))
	fmt.Printf("issued %s for %s (%s, %d nodes, expires %s) -> %s\n", *licID, *customer, *edition, *nodes, exp.Format("2006-01-02"), *out)
}

func inspect(args []string) {
	if len(args) != 1 {
		die("usage: inspect <file.lic>")
	}
	b, err := os.ReadFile(args[0])
	check(err)
	p, err := license.Verify(string(b), license.BuiltinKeyRing())
	if err != nil {
		fmt.Println("signature:", err)
		os.Exit(1)
	}
	j, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println("signature: OK")
	fmt.Println(string(j))
}

func loadKey(path string) ed25519.PrivateKey {
	b, err := os.ReadFile(path)
	check(err)
	blk, _ := pem.Decode(b)
	if blk == nil {
		die("bad key file")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	check(err)
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		die("key is not ed25519")
	}
	return priv
}

func check(err error) {
	if err != nil {
		die(err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: syapp-license-server keygen | issue | inspect | version")
	os.Exit(2)
}
