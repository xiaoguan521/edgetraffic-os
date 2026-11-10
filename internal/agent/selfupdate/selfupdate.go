// Package selfupdate 是 agent 自升级（规格书 12.6：agent 自升级使用同一种签名包，只重启 agent 进程）。
//
// 新版本安装在状态目录 agent/<版本>/ 下，连同升级包与签名一起保存；/usr/bin/syedge-agent 启动时（MaybeExec）
// 再次校验签名与二进制哈希，只有版本更新且校验通过才 exec 到新版本。agent 以普通用户运行，自升级不涉及提权；
// 未签名或被改动的二进制永远不会被执行。
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

const (
	maxBinary = 96 << 20
	envGuard  = "ETOS_AGENT_SELFUPDATE_EXEC"
)

// binEntry 是 agent 升级包中二进制的相对路径。
var binEntry = "bin/" + brand.Agent()

var ErrRejected = errors.New("agent package rejected")

func dir(state, ver string) string { return filepath.Join(state, "agent", ver) }

// extract 校验签名并取出 bin/syedge-agent。
func extract(data, sigJSON []byte, arch string, keys map[string]ed25519.PublicKey) (*pkgsig.Sig, []byte, error) {
	sig, err := pkgsig.Parse(sigJSON)
	if err != nil {
		return nil, nil, err
	}
	if sig.Name != brand.Agent() || sig.Arch != arch {
		return nil, nil, fmt.Errorf("%w: signature is for %s %s", ErrRejected, sig.Name, sig.Arch)
	}
	if err := pkgsig.Verify(sig, data, keys); err != nil {
		return nil, nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrRejected, err)
		}
		if strings.TrimPrefix(h.Name, "./") == binEntry && h.Typeflag == tar.TypeReg && h.Size <= maxBinary {
			b, err := io.ReadAll(io.LimitReader(tr, maxBinary))
			return sig, b, err
		}
	}
	return nil, nil, fmt.Errorf("%w: package has no %s", ErrRejected, binEntry)
}

// Install 校验并安装新版本，切换 agent/current 指向它。
func Install(state string, data, sigJSON []byte, arch string, keys map[string]ed25519.PublicKey) (string, error) {
	sig, bin, err := extract(data, sigJSON, arch, keys)
	if err != nil {
		return "", err
	}
	d := dir(state, sig.Version)
	tmp := d + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "package.tar.gz"), data, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "package.sig"), sigJSON, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, brand.Agent()), bin, 0o700); err != nil {
		return "", err
	}
	_ = os.RemoveAll(d)
	if err := os.Rename(tmp, d); err != nil {
		return "", err
	}
	link := filepath.Join(state, "agent", "current")
	_ = os.Remove(link + ".new")
	if err := os.Symlink(sig.Version, link+".new"); err != nil {
		return "", err
	}
	return sig.Version, os.Rename(link+".new", link)
}

// newer 比较 N.N.N[-N]。
func newer(a, b string) bool {
	pa, pb := parts(a), parts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [4]int {
	var out [4]int
	main, rel, _ := strings.Cut(v, "-")
	for i, s := range strings.SplitN(main, ".", 3) {
		out[i], _ = strconv.Atoi(s)
	}
	out[3], _ = strconv.Atoi(rel)
	return out
}

// Staged 返回已安装且校验通过、比 running 新的版本的二进制路径；没有则返回空串。
func Staged(state, running, arch string, keys map[string]ed25519.PublicKey) string {
	ver, err := os.Readlink(filepath.Join(state, "agent", "current"))
	if err != nil || !pkgsig.VersionRe.MatchString(ver) || (pkgsig.VersionRe.MatchString(running) && !newer(ver, running)) {
		return ""
	}
	d := dir(state, ver)
	data, err1 := os.ReadFile(filepath.Join(d, "package.tar.gz"))
	sigJSON, err2 := os.ReadFile(filepath.Join(d, "package.sig"))
	bin, err3 := os.ReadFile(filepath.Join(d, brand.Agent()))
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	sig, want, err := extract(data, sigJSON, arch, keys)
	if err != nil || sig.Version != ver {
		return ""
	}
	h1, h2 := sha256.Sum256(want), sha256.Sum256(bin)
	if hex.EncodeToString(h1[:]) != hex.EncodeToString(h2[:]) {
		return "" // 二进制被改动：不执行
	}
	return filepath.Join(d, brand.Agent())
}

// stagedCompatible refuses a signed binary that cannot accept the config argument
// the current process would pass through on exec.
func stagedCompatible(state, running, arch string, keys map[string]ed25519.PublicKey, requireConfig bool) (string, error) {
	bin := Staged(state, running, arch, keys)
	if bin == "" || !requireConfig {
		return bin, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Both forms are supported by the public CLI and may appear in os.Args.
	for _, args := range [][]string{{"run", "-c", "/dev/null", "-h"}, {"--config", "/dev/null", "run", "-h"}} {
		if err := exec.CommandContext(ctx, bin, args...).Run(); err != nil {
			return "", fmt.Errorf("staged agent does not support the specified config option: %w", err)
		}
	}
	return bin, nil
}

// MaybeExec 在启动时切换到已安装的新版本（只执行一次，避免循环）。返回时表示继续运行当前二进制。
func MaybeExec(state, running, arch string, keys map[string]ed25519.PublicKey, requireConfig bool) error {
	if os.Getenv(envGuard) != "" {
		return nil
	}
	bin, err := stagedCompatible(state, running, arch, keys, requireConfig)
	if err != nil {
		return err
	}
	if bin == "" {
		return nil
	}
	env := append(os.Environ(), envGuard+"=1")
	return syscall.Exec(bin, append([]string{bin}, os.Args[1:]...), env)
}
