// syapp-license-server：厂商授权工具（规格书 8.8）。部署在厂商内网，不交付给客户。
//
// 这里的 keygen / issue 使用明文私钥文件，只用于演示（etos-demo-*）与 CI 临时密钥（etos-ci-*）。
// 正式 License 一律由离线签名机上的 syapp-license-signer 签名（规格书 8.8.1，docs/license-ops.md）。
//
//	syapp-license-server keygen -kid etos-demo-2027 -out keys/
//	syapp-license-server issue  -key keys/etos-demo-2026.key -kid etos-demo-2026 \
//	    -request activation-request.json -customer "某某公司" -edition professional \
//	    -nodes 50 -sites 2000 -days 365 [-binding installation_only] -out customer.lic
//	syapp-license-server inspect customer.lic
//
// 正式签发（不接触私钥，授权后台上线前的命令行流程，规格书 8.8.1）：
//
//	syapp-license-server request     -batch B0001 -kid etos-prod-2027 -request activation-request.json -customer "某某公司" ... -out sign-request-B0001.json
//	syapp-license-server crl-request -batch B0002 -kid etos-prod-2027 -serial 1 -revoke LIC-...,LIC-... [-base current.crl] -out sign-request-B0002.json
//	（U 盘拷到离线签名机：syapp-license-signer sign ...，再拷回）
//	syapp-license-server import      -request sign-request-B0001.json -signed signed-B0001.json -out licenses/
//
// 授权后台 Web（客户、订单、审批、签名批次、吊销、审计；只持有公钥）：
//
//	syapp-license-server serve -listen 0.0.0.0:8600 -data /var/lib/syapp-license-server -tls-auto
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
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

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
	case "request":
		check(cmdRequest(os.Args[2:], os.Stdout))
	case "crl-request":
		check(cmdCRLRequest(os.Args[2:], os.Stdout, license.BuiltinKeyRing()))
	case "sign-package":
		signPackage(os.Args[2:])
	case "serve":
		check(cmdServe(os.Args[2:]))
	case "import":
		check(cmdImport(os.Args[2:], os.Stdout, license.BuiltinKeyRing()))
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
	if !demoKID(*kid) {
		die("syapp-license-server keygen 只生成演示 / CI 密钥（etos-demo-* / etos-ci-*）；正式密钥请在离线签名机上用 syapp-license-signer keygen 生成")
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
	instID := fs.String("installation-id", "", "直接指定安装 ID（替代 -request，仅配合 -binding installation_only 使用）")
	customer := fs.String("customer", "", "客户名称")
	customerID := fs.String("customer-id", "", "客户编号")
	edition := fs.String("edition", "professional", "trial | standard | professional | enterprise")
	nodes := fs.Int("nodes", 10, "节点数上限（0 表示不限）")
	sites := fs.Int("sites", 200, "站点数上限（0 表示不限）")
	days := fs.Int("days", 365, "有效天数")
	expires := fs.String("expires", "", "到期日 YYYY-MM-DD（覆盖 -days，可用于签发已过期的演示授权）")
	grace := fs.Int("grace", 15, "宽限天数")
	maint := fs.String("maintenance", "", "维护截止日 YYYY-MM-DD（默认同到期日）；构建日期晚于它的控制台视授权无效")
	binding := fs.String("binding", "installation", "installation（安装 ID + 主机指纹）| installation_only（容器部署）")
	licID := fs.String("id", "", "License 编号（默认自动生成）")
	out := fs.String("out", "", "输出文件（默认打印到标准输出）")
	_ = fs.Parse(args)
	if *keyPath == "" || *kid == "" || *customer == "" || (*reqPath == "" && *instID == "") {
		die("-key, -kid, -customer and one of -request / -installation-id are required")
	}
	if *reqPath == "" && *binding != "installation_only" {
		die("-installation-id requires -binding installation_only (host fingerprint comes from the activation request)")
	}
	if !demoKID(*kid) {
		die("syapp-license-server issue 只能用演示 / CI 密钥签发；正式 License 由离线签名机上的 syapp-license-signer 签名（docs/license-ops.md）")
	}
	priv := loadKey(*keyPath)
	var req license.ActivationRequest
	if *reqPath != "" {
		b, err := os.ReadFile(*reqPath)
		check(err)
		check(json.Unmarshal(b, &req))
	} else {
		req.InstallationID = *instID
	}
	o := license.IssueOptions{KID: *kid, LicenseID: *licID, CustomerID: *customerID, CustomerName: *customer,
		Edition: license.Edition(*edition), Nodes: *nodes, Sites: *sites, Days: *days, GraceDays: *grace,
		Binding: *binding, Request: req, ProductName: version.Product, Now: time.Now()}
	if *expires != "" {
		t, err := time.Parse("2006-01-02", *expires)
		check(err)
		o.ExpiresAt = t
	}
	if *maint != "" {
		t, err := time.Parse("2006-01-02", *maint)
		check(err)
		o.Maintenance = t
	}
	p, err := license.BuildPayload(o)
	check(err)
	raw, err := license.Sign(p, priv)
	check(err)
	if *out == "" {
		fmt.Println(raw)
		return
	}
	check(os.WriteFile(*out, []byte(raw+"\n"), 0o644))
	fmt.Printf("issued %s for %s (%s, %d nodes, expires %s) -> %s\n", p.LicenseID, *customer, *edition, *nodes, p.ExpiresAt.Format("2006-01-02"), *out)
}

// signPackage 用演示 / CI 密钥为升级包签名（正式升级包由离线签名机上的 syapp-license-signer sign-package 签名）。
func signPackage(args []string) {
	fs := flag.NewFlagSet("sign-package", flag.ExitOnError)
	keyPath := fs.String("key", "", "演示 / CI 升级包签名私钥（PEM）")
	kid := fs.String("kid", "", "密钥编号（etos-demo-pkg-* / etos-ci-pkg-*）")
	in := fs.String("in", "", "升级包 tar.gz")
	name := fs.String("name", "syapp", "syapp | syapp-agent")
	ver := fs.String("version", "", "版本号")
	arch := fs.String("arch", "", "amd64 | arm64")
	variant := fs.String("variant", "standard", "standard | gmssl | lua")
	_ = fs.Parse(args)
	if !strings.HasPrefix(*kid, "etos-demo-pkg-") && !strings.HasPrefix(*kid, "etos-ci-pkg-") {
		die("syapp-license-server sign-package 只能用演示 / CI 升级包密钥；正式升级包请在离线签名机上用 syapp-license-signer sign-package 签名")
	}
	data, err := os.ReadFile(*in)
	check(err)
	sig, err := pkgsig.Sign(loadKey(*keyPath), *kid, *name, *ver, *arch, *variant, data)
	check(err)
	b, _ := json.MarshalIndent(sig, "", "  ")
	check(os.WriteFile(*in+".sig", append(b, '\n'), 0o644))
	fmt.Printf("signed %s %s %s %s (sha256 %s) -> %s.sig\n", *name, *ver, *arch, *variant, sig.SHA256, *in)
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

// demoKID 报告是否为演示或 CI 临时密钥。
func demoKID(kid string) bool {
	return strings.HasPrefix(kid, "etos-demo-") || kid == license.CIKeyID || strings.HasPrefix(kid, "etos-ci-")
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
	fmt.Fprintln(os.Stderr, "usage: syapp-license-server serve | request | crl-request | import | keygen | issue | sign-package | inspect | version")
	os.Exit(2)
}
