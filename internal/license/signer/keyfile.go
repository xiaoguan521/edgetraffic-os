// Package signer 是离线签名机上使用的代码（规格书 8.8.1）：口令加密的私钥文件、批次签名、本地签发日志。
// 只被 cmd/syapp-license-signer 引用；控制台与授权后台都不链接这个包。
package signer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/xiaoguan521/edgetraffic-os/internal/license"
)

// KeyFileFormat 标识私钥文件格式。
const KeyFileFormat = "etos-signing-key-v1"

// MinPassphrase 是口令的最短长度。
const MinPassphrase = 16

// KDF 记录口令派生参数（随文件保存，便于将来调高参数）。
type KDF struct {
	Alg       string `json:"alg"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	Salt      string `json:"salt"`
}

// KeyFile 是磁盘上的私钥文件：私钥种子用口令派生的密钥以 AES-256-GCM 加密，
// 格式、kid 与公钥作为附加认证数据，任何一项被改动都无法解密。明文私钥从不写入磁盘。
type KeyFile struct {
	Format     string    `json:"format"`
	KID        string    `json:"kid"`
	PublicKey  string    `json:"public_key"`
	KDF        KDF       `json:"kdf"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
	CreatedAt  time.Time `json:"created_at"`
}

// ProductionKDF 是生成私钥文件时使用的口令派生参数。离线签名机上运行，参数取得较高：3 轮、256 MiB。
var ProductionKDF = KDF{Alg: "argon2id", Time: 3, MemoryKiB: 256 * 1024, Threads: 4}

// 生成与打开私钥文件时使用的参数和下限。只有本包的测试会临时调低它们（见 TestMain），
// 生产代码没有任何开关可以改变它们。
var (
	kdfParams    = ProductionKDF
	minMemoryKiB = uint32(64 * 1024)
)

func (k *KeyFile) aad() []byte { return []byte(k.Format + "\n" + k.KID + "\n" + k.PublicKey) }

func deriveKey(pass []byte, kdf KDF) ([]byte, error) {
	if kdf.Alg != "argon2id" || kdf.Time == 0 || kdf.MemoryKiB < minMemoryKiB || kdf.Threads == 0 {
		return nil, errors.New("私钥文件的口令派生参数不合法")
	}
	salt, err := base64.StdEncoding.DecodeString(kdf.Salt)
	if err != nil || len(salt) < 16 {
		return nil, errors.New("私钥文件的 salt 不合法")
	}
	return argon2.IDKey(pass, salt, kdf.Time, kdf.MemoryKiB, kdf.Threads, 32), nil
}

// CheckPassphrase 检查口令强度。
func CheckPassphrase(pass []byte) error {
	if len([]rune(string(pass))) < MinPassphrase {
		return fmt.Errorf("口令至少 %d 个字符", MinPassphrase)
	}
	return nil
}

// Generate 生成一把新密钥，返回加密后的私钥文件与公钥。
func Generate(kid string, pass []byte, now time.Time) (*KeyFile, ed25519.PublicKey, error) {
	if err := CheckPassphrase(pass); err != nil {
		return nil, nil, err
	}
	if !strings.HasPrefix(kid, "etos-prod-") {
		return nil, nil, errors.New("正式密钥编号必须形如 etos-prod-<年份>，例如 etos-prod-2027")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	seed := priv.Seed()
	defer wipe(seed)
	defer wipe(priv)
	kdf := kdfParams
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	kdf.Salt = base64.StdEncoding.EncodeToString(salt)
	kf := &KeyFile{Format: KeyFileFormat, KID: kid, PublicKey: base64.StdEncoding.EncodeToString(pub), KDF: kdf, CreatedAt: now.UTC()}
	key, err := deriveKey(pass, kdf)
	if err != nil {
		return nil, nil, err
	}
	defer wipe(key)
	aead, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	kf.Nonce = base64.StdEncoding.EncodeToString(nonce)
	kf.Ciphertext = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, seed, kf.aad()))
	return kf, pub, nil
}

// ParseKeyFile 读取私钥文件（不需要口令）。
func ParseKeyFile(b []byte) (*KeyFile, error) {
	var kf KeyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("不是私钥文件：%w", err)
	}
	if kf.Format != KeyFileFormat || kf.KID == "" {
		return nil, errors.New("不是 EdgeTraffic OS 签名私钥文件")
	}
	if _, err := kf.Public(); err != nil {
		return nil, err
	}
	return &kf, nil
}

// Public 返回文件中记录的公钥。
func (k *KeyFile) Public() (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("私钥文件中的公钥不合法")
	}
	return ed25519.PublicKey(b), nil
}

// Open 用口令解密私钥，并核对它与文件中记录的公钥一致。调用方用完后应调用 Wipe。
func (k *KeyFile) Open(pass []byte) (ed25519.PrivateKey, error) {
	key, err := deriveKey(pass, k.KDF)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce, err1 := base64.StdEncoding.DecodeString(k.Nonce)
	ct, err2 := base64.StdEncoding.DecodeString(k.Ciphertext)
	if err1 != nil || err2 != nil || len(nonce) != aead.NonceSize() {
		return nil, errors.New("私钥文件已损坏")
	}
	seed, err := aead.Open(nil, nonce, ct, k.aad())
	if err != nil {
		return nil, errors.New("口令错误，或私钥文件已被改动")
	}
	defer wipe(seed)
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("私钥文件已损坏")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, _ := k.Public()
	if !pub.Equal(priv.Public()) {
		Wipe(priv)
		return nil, errors.New("私钥与文件中记录的公钥不一致")
	}
	return priv, nil
}

// Fingerprint 返回公钥指纹（与授权后台显示的格式相同），密钥仪式上两人分别核对。
func Fingerprint(pub ed25519.PublicKey) string { return license.KeyFingerprint(pub) }

// Wipe 清零私钥内存（尽力而为）。
func Wipe(priv ed25519.PrivateKey) { wipe(priv) }

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func gcm(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}
