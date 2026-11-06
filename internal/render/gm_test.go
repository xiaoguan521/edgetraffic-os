package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/sm2cert/sm2certtest"
)

func gmPair(t *testing.T, domain string) *GMPair {
	t.Helper()
	s := sm2certtest.New(t, domain, "sign", []string{domain})
	e := sm2certtest.New(t, domain, "enc", []string{domain})
	return &GMPair{SignCertPEM: s.CertPEM, SignKeyPEM: s.KeyPEM, EncCertPEM: e.CertPEM, EncKeyPEM: e.KeyPEM}
}

func gmInput(t *testing.T) Input {
	in := sampleInput(t)
	in.Sites[0].HTTPS.GM = gmPair(t, "shop.example.com")
	return in
}

func TestGMRender(t *testing.T) {
	plain, err := Render(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	for n, b := range plain.Files {
		if strings.Contains(string(b), "ssl_conf_command") {
			t.Fatalf("%s: GM directives without a GM site", n)
		}
	}

	in := gmInput(t)
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sm2-sign.crt", "sm2-sign.key", "sm2-enc.crt", "sm2-enc.key"} {
		if len(b.Files["certs/site-shop."+f]) == 0 {
			t.Fatalf("missing certs/site-shop.%s", f)
		}
	}
	site := string(b.Files["conf.d/site-shop.conf"])
	for _, want := range []string{
		"ssl_conf_command  Enable_ntls  on;",
		"ssl_conf_command  SignCertificate  /opt/syedge/conf/releases/r000002/certs/site-shop.sm2-sign.crt;",
		"ssl_conf_command  SignPrivateKey   /opt/syedge/conf/releases/r000002/certs/site-shop.sm2-sign.key;",
		"ssl_conf_command  EncCertificate   /opt/syedge/conf/releases/r000002/certs/site-shop.sm2-enc.crt;",
		"ssl_conf_command  EncPrivateKey    /opt/syedge/conf/releases/r000002/certs/site-shop.sm2-enc.key;",
		"ECC-SM2-SM4-GCM-SM3",
		"ssl_certificate      certs/site-shop.crt;", // 标准证书继续服务不支持国密的客户端
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site conf lacks %q:\n%s", want, site)
		}
	}
	// 默认 443 server 持有握手最初的 SSL 对象：国密握手在 SNI 切换前识别，必须同样开启
	if !strings.Contains(string(b.Files["syedge.conf"]), "ssl_conf_command  Enable_ntls  on;") {
		t.Error("default 443 server lacks Enable_ntls")
	}
	if strings.Contains(string(b.Files["conf.d/site-blog.conf"]), "ssl_conf_command") {
		t.Error("non-GM site got GM directives")
	}
}

func TestGMValidation(t *testing.T) {
	bad := []func(*Input){
		func(in *Input) { in.Sites[0].HTTPS.GM.SignCertPEM = "x" },
		func(in *Input) { in.Sites[0].HTTPS.GM.EncKeyPEM = "" },
		func(in *Input) {
			in.Sites[0].HTTPS.GM.EncCertPEM = in.Sites[0].HTTPS.CertPEM + "\nssl_conf_command x y;"
		},
	}
	for i, f := range bad {
		in := gmInput(t)
		f(&in)
		if _, err := Render(in); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// 证书续期在线上版本基础上重打包：国密指令里的版本目录路径随新版本改写，
// agent 的“只换证书”判断只容许这一处精确改写。
func TestGMCertOnlyChange(t *testing.T) {
	cur, err := Render(gmInput(t))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for n, b := range cur.Files {
		files[n] = b
	}
	np := gmPair(t, "shop.example.com")
	files["certs/site-shop.sm2-sign.crt"] = []byte(np.SignCertPEM)
	files["certs/site-shop.sm2-sign.key"] = []byte(np.SignKeyPEM)
	next := Repack("r000003", RebaseRevision(files, "r000002", "r000003"))
	if !strings.Contains(string(next.Files["conf.d/site-shop.conf"]), "/releases/r000003/certs/site-shop.sm2-sign.crt") {
		t.Fatal("paths not rebased")
	}
	if !CertOnlyChange(cur.Files, next.Files) {
		t.Fatal("rebased renewal not recognised as cert-only")
	}
	// 没改写路径（指向旧版本目录）不算合法续期
	if CertOnlyChange(cur.Files, Repack("r000003", files).Files) {
		t.Fatal("stale release path accepted")
	}
	// 改写成其他版本目录，或夹带其他改动，都不行
	other := RebaseRevision(files, "r000002", "r000001")
	if CertOnlyChange(cur.Files, Repack("r000003", other).Files) {
		t.Fatal("path to another revision accepted")
	}
	sneaky := RebaseRevision(files, "r000002", "r000003")
	sneaky["conf.d/site-shop.conf"] = append(sneaky["conf.d/site-shop.conf"], []byte("\n# x")...)
	if CertOnlyChange(cur.Files, Repack("r000003", sneaky).Files) {
		t.Fatal("extra change accepted")
	}
}

// 国密变体（铜锁构建）的 syedge -t：SYEDGE_GM_BIN 指向国密版 syedge 时执行（CI 的 dataplane-gm 任务）。
// ssl_conf_command 的文件路径是绝对路径，因此版本必须写到 /opt/syedge/conf/releases/<版本> 下。
func TestRenderGMSyedgeSyntax(t *testing.T) {
	bin := os.Getenv("SYEDGE_GM_BIN")
	if bin == "" {
		t.Skip("SYEDGE_GM_BIN not set")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	in := gmInput(t)
	in.Revision = "r900001"
	if mt, err := os.ReadFile("/opt/syedge/share/defaults/mime.types"); err == nil {
		in.MimeTypes = mt
	}
	// 两个国密站点共用 443：SNI 切换后各自的双证书都要加载
	in.Sites[1].HTTPS = selfSigned(t, "blog.example.com")
	in.Sites[1].HTTPS.GM = gmPair(t, "blog.example.com")
	if cert := os.Getenv("GM_SIGN_CERT"); cert != "" { // CI 用铜锁签发的真实证书（可握手）
		rd := func(k string) string { b, _ := os.ReadFile(os.Getenv(k)); return string(b) }
		in.Sites[0].HTTPS.GM = &GMPair{SignCertPEM: rd("GM_SIGN_CERT"), SignKeyPEM: rd("GM_SIGN_KEY"), EncCertPEM: rd("GM_ENC_CERT"), EncKeyPEM: rd("GM_ENC_KEY")}
	}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("/opt/syedge/conf/releases", in.Revision)
	_ = os.RemoveAll(dir)
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-t", "-q", "-c", filepath.Join(dir, "syedge.conf")).CombinedOutput(); err != nil {
		t.Fatalf("syedge -t failed: %v\n%s", err, out)
	}
	if os.Getenv("GM_KEEP") == "" {
		_ = os.RemoveAll(dir)
	}
}
