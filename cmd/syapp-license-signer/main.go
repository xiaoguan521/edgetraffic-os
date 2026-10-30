// syapp-license-signer：离线签名工具（规格书 8.8.1）。只在物理断网的签名机上运行。
//
// 程序不含任何网络代码（TestNoNetworkCode 检查依赖），私钥只以口令加密的文件形式存在。
//
//	syapp-license-signer keygen  -kid etos-prod-2027 -out /media/usb [-two-person]
//	syapp-license-signer pubkey  -key etos-prod-2027.key.json
//	syapp-license-signer inspect -in sign-request-B0012.json
//	syapp-license-signer sign    -key etos-prod-2027.key.json -in sign-request-B0012.json [-out signed-B0012.json] [-log signer-log.jsonl] [-two-person]
//	syapp-license-signer verify  -key etos-prod-2027.key.json -request sign-request-B0012.json -signed signed-B0012.json
//	syapp-license-signer sign-package -key etos-pkg-2027.key.json -in syapp-1.30.7-1-amd64.tar.gz -name syapp -version 1.30.7-1 -arch amd64 [-variant standard]
package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/license/signer"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

type app struct {
	in     *bufio.Reader
	out    io.Writer
	errw   io.Writer
	now    func() time.Time
	prompt func(label string) ([]byte, error) // 读取口令（终端不回显）
	sysNet string                             // /sys/class/net，测试时替换
}

func main() {
	a := &app{in: bufio.NewReader(os.Stdin), out: os.Stdout, errw: os.Stderr, now: time.Now, sysNet: "/sys/class/net"}
	a.prompt = func(label string) ([]byte, error) {
		fmt.Fprint(os.Stderr, label)
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return nil, errors.New("请在终端中输入口令（或用 -passphrase-file）")
		}
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return b, err
	}
	os.Exit(a.run(os.Args[1:]))
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		a.usage()
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = a.keygen(args[1:])
	case "pubkey":
		err = a.pubkey(args[1:])
	case "inspect":
		err = a.inspect(args[1:])
	case "sign":
		err = a.sign(args[1:])
	case "verify":
		err = a.verify(args[1:])
	case "sign-package":
		err = a.signPackage(args[1:])
	case "version", "-version":
		fmt.Fprintln(a.out, version.String("license-signer"))
	default:
		a.usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(a.errw, "错误：", err)
		return 1
	}
	return 0
}

func (a *app) usage() {
	fmt.Fprintln(a.errw, `用法：syapp-license-signer keygen|pubkey|inspect|sign|verify|sign-package [参数]   （-h 查看各子命令参数）`)
}

// offlineCheck 在 Linux 上检查是否有处于连接状态的网卡（只读 /sys，不使用网络库）。
func (a *app) offlineCheck(require bool) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	ents, err := os.ReadDir(a.sysNet)
	if err != nil {
		return nil
	}
	var up []string
	for _, e := range ents {
		if e.Name() == "lo" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.sysNet, e.Name(), "operstate"))
		if err == nil && strings.TrimSpace(string(b)) == "up" {
			up = append(up, e.Name())
		}
	}
	if len(up) == 0 {
		return nil
	}
	msg := fmt.Sprintf("检测到网络接口处于连接状态：%s。离线签名机应拔掉网线并关闭无线", strings.Join(up, "、"))
	if require {
		return errors.New(msg)
	}
	fmt.Fprintln(a.errw, "警告："+msg)
	return nil
}

func (a *app) passphrase(file string, twoPerson, confirm bool) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimRight(string(b), "\r\n")), nil
	}
	read := func(label string) ([]byte, error) {
		p, err := a.prompt(label)
		if err != nil {
			return nil, err
		}
		if confirm {
			q, err := a.prompt("再次输入：")
			if err != nil {
				return nil, err
			}
			if string(p) != string(q) {
				return nil, errors.New("两次输入不一致")
			}
		}
		return p, nil
	}
	if !twoPerson {
		return read("私钥口令：")
	}
	// 双人规则：两人各输入自己的一半，每半至少 8 个字符
	p1, err := read("第一人口令（另一人回避）：")
	if err != nil {
		return nil, err
	}
	p2, err := read("第二人口令（另一人回避）：")
	if err != nil {
		return nil, err
	}
	if len([]rune(string(p1))) < 8 || len([]rune(string(p2))) < 8 {
		return nil, errors.New("双人口令每一半至少 8 个字符")
	}
	return append(append(p1, '\n'), p2...), nil
}

func (a *app) keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	kid := fs.String("kid", "", "密钥编号，形如 etos-prod-2027")
	outDir := fs.String("out", ".", "私钥文件输出目录（建议直接写到 U 盘）")
	twoPerson := fs.Bool("two-person", false, "双人规则：口令由两人各持一半")
	passFile := fs.String("passphrase-file", "", "从文件读取口令（仅用于演练与测试）")
	requireOffline := fs.Bool("require-offline", false, "检测到联网时拒绝运行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.offlineCheck(*requireOffline); err != nil {
		return err
	}
	keyPath := filepath.Join(*outDir, *kid+".key.json")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%s 已存在，不会覆盖", keyPath)
	}
	pass, err := a.passphrase(*passFile, *twoPerson, true)
	if err != nil {
		return err
	}
	kf, pub, err := signer.Generate(*kid, pass, a.now())
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	if err := writeNew(keyPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fp := signer.Fingerprint(pub)
	pubTxt := fmt.Sprintf("kid: %s\npublic_key: %s\nfingerprint: %s\ncreated_at: %s\n", kf.KID, kf.PublicKey, fp, kf.CreatedAt.Format(time.RFC3339))
	pubPath := filepath.Join(*outDir, *kid+".pub.txt")
	if err := writeNew(pubPath, []byte(pubTxt), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "加密私钥：%s（复制到两个 U 盘分开保管；口令与 U 盘分开保管）\n", keyPath)
	fmt.Fprintf(a.out, "公钥信息：%s（可以带出离线机）\n\n", pubPath)
	fmt.Fprintf(a.out, "公钥指纹（两人分别核对并签字）：\n    %s\n\n", fp)
	fmt.Fprintf(a.out, "写入 internal/license/keys.go 的 builtinKeys：\n    %q: %q,\n", kf.KID, kf.PublicKey)
	return nil
}

func (a *app) loadKey(path string) (*signer.KeyFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return signer.ParseKeyFile(b)
}

func (a *app) pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	keyPath := fs.String("key", "", "私钥文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kf, err := a.loadKey(*keyPath)
	if err != nil {
		return err
	}
	pub, _ := kf.Public()
	fmt.Fprintf(a.out, "kid: %s\npublic_key: %s\nfingerprint: %s\n", kf.KID, kf.PublicKey, signer.Fingerprint(pub))
	return nil
}

func (a *app) readRequest(path string) ([]byte, *license.SignRequest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	req, err := license.ParseSignRequest(raw, a.now())
	if err != nil {
		return nil, nil, err
	}
	return raw, req, nil
}

func (a *app) inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	in := fs.String("in", "", "签名请求文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, req, err := a.readRequest(*in)
	if err != nil {
		return err
	}
	fmt.Fprint(a.out, signer.Summary(req))
	fmt.Fprintf(a.out, "\n请求 SHA-256：%s\n", license.Digest(raw))
	return nil
}

func (a *app) sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "私钥文件")
	in := fs.String("in", "", "签名请求文件 sign-request-<批次>.json")
	out := fs.String("out", "", "签名结果文件（默认 signed-<批次>.json，与请求同目录）")
	logPath := fs.String("log", "signer-log.jsonl", "本地签发日志（只追加）")
	twoPerson := fs.Bool("two-person", false, "双人规则")
	passFile := fs.String("passphrase-file", "", "从文件读取口令（仅用于演练与测试）")
	yes := fs.Bool("yes", false, "跳过逐条确认（仅用于演练与测试）")
	requireOffline := fs.Bool("require-offline", false, "检测到联网时拒绝运行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.offlineCheck(*requireOffline); err != nil {
		return err
	}
	kf, err := a.loadKey(*keyPath)
	if err != nil {
		return err
	}
	raw, req, err := a.readRequest(*in)
	if err != nil {
		return err
	}
	if req.KID != kf.KID {
		return fmt.Errorf("批次要求密钥 %s，当前私钥文件是 %s", req.KID, kf.KID)
	}
	sha := license.Digest(raw)
	if signer.Signed(*logPath, sha) {
		return fmt.Errorf("这份请求（SHA-256 %s…）已经签过，见 %s", sha[:12], *logPath)
	}
	fmt.Fprint(a.out, signer.Summary(req))
	fmt.Fprintf(a.out, "\n请求 SHA-256：%s\n", sha)
	if !*yes {
		fmt.Fprintf(a.out, "\n逐条核对无误后，输入批次号 %s 确认签名：", req.Batch)
		line, _ := a.in.ReadString('\n')
		if strings.TrimSpace(line) != req.Batch {
			return errors.New("未确认，已取消")
		}
	}
	pass, err := a.passphrase(*passFile, *twoPerson, false)
	if err != nil {
		return err
	}
	priv, err := kf.Open(pass)
	if err != nil {
		return err
	}
	defer signer.Wipe(priv)
	res, err := signer.SignBatch(req, raw, kf.KID, priv, a.now())
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(filepath.Dir(*in), "signed-"+req.Batch+".json")
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := writeNew(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := signer.AppendLog(*logPath, req, res); err != nil {
		return fmt.Errorf("签名结果已写入 %s，但签发日志写入失败：%w", *out, err)
	}
	fmt.Fprintf(a.out, "\n已签名 %d 个 License", len(res.Licenses))
	if res.CRL != "" {
		fmt.Fprintf(a.out, "、1 份吊销列表")
	}
	fmt.Fprintf(a.out, " → %s\n签发日志：%s\n", *out, *logPath)
	return nil
}

func (a *app) verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	keyPath := fs.String("key", "", "私钥文件（只用其中的公钥，不需要口令）")
	reqPath := fs.String("request", "", "签名请求文件")
	signedPath := fs.String("signed", "", "签名结果文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kf, err := a.loadKey(*keyPath)
	if err != nil {
		return err
	}
	raw, req, err := a.readRequest(*reqPath)
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
	pub, _ := kf.Public()
	if err := license.VerifySignedBatch(req, raw, &s, license.KeyRing{kf.KID: pub}); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "核对通过：批次 %s，%d 个 License\n", s.Batch, len(s.Licenses))
	return nil
}

// writeNew 只创建新文件，不覆盖已有文件。
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// signPackage 为数据面 / agent 升级包签名（规格书 12.6）。只接受 etos-pkg-* 密钥，与 License 签名密钥分开。
func (a *app) signPackage(args []string) error {
	fs := flag.NewFlagSet("sign-package", flag.ContinueOnError)
	keyPath := fs.String("key", "", "升级包签名私钥文件（etos-pkg-*）")
	in := fs.String("in", "", "升级包（tar.gz）")
	name := fs.String("name", "syapp", "syapp | syapp-agent")
	ver := fs.String("version", "", "版本号，如 1.30.7-1")
	arch := fs.String("arch", "", "amd64 | arm64")
	variant := fs.String("variant", "standard", "standard | gmssl | lua")
	twoPerson := fs.Bool("two-person", false, "双人规则")
	passFile := fs.String("passphrase-file", "", "从文件读取口令（仅用于演练与测试）")
	yes := fs.Bool("yes", false, "跳过确认（仅用于演练与测试）")
	requireOffline := fs.Bool("require-offline", false, "检测到联网时拒绝运行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.offlineCheck(*requireOffline); err != nil {
		return err
	}
	kf, err := a.loadKey(*keyPath)
	if err != nil {
		return err
	}
	if !pkgsig.KIDRe.MatchString(kf.KID) {
		return fmt.Errorf("升级包只能用 etos-pkg-* 密钥签名（当前 %s）；License 签名密钥不能用于升级包", kf.KID)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "升级包：%s\n名称：%s  版本：%s  架构：%s  变体：%s\n大小：%d 字节\n", filepath.Base(*in), *name, *ver, *arch, *variant, len(data))
	if !*yes {
		fmt.Fprintf(a.out, "\n核对构建记录（版本与 SHA-256 见 inspect 输出）后，输入版本号 %s 确认签名：", *ver)
		line, _ := a.in.ReadString('\n')
		if strings.TrimSpace(line) != *ver {
			return errors.New("未确认，已取消")
		}
	}
	pass, err := a.passphrase(*passFile, *twoPerson, false)
	if err != nil {
		return err
	}
	priv, err := kf.Open(pass)
	if err != nil {
		return err
	}
	defer signer.Wipe(priv)
	sig, err := pkgsig.Sign(priv, kf.KID, *name, *ver, *arch, *variant, data)
	if err != nil {
		return err
	}
	pub, _ := kf.Public()
	if err := pkgsig.Verify(sig, data, map[string]ed25519.PublicKey{kf.KID: pub}); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(sig, "", "  ")
	if err := writeNew(*in+".sig", append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "SHA-256：%s\n签名 → %s.sig\n", sig.SHA256, *in)
	return nil
}
