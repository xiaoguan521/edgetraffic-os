// Package render 把结构化的站点模型编译成 syapp 配置目录（规格书第 6 节）。
//
// 规则：任何用户输入都必须先经本文件的校验器，再经模板输出；
// 禁止用字符串拼接把用户输入写进配置。
package render

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

var (
	ErrDomain   = errors.New("render: invalid domain")
	ErrPath     = errors.New("render: invalid path")
	ErrUpstream = errors.New("render: upstream not allowed")
	ErrName     = errors.New("render: invalid name")
)

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidateName 校验站点名、upstream 名等会进入文件名或配置标识符的名字。
func ValidateName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("%w: %q", ErrName, s)
	}
	return nil
}

// ValidateDomain 接受小写 FQDN 或单层通配 *.example.com；拒绝裸 *。
func ValidateDomain(d string) error {
	if d == "" || len(d) > 253 || d != strings.ToLower(d) {
		return fmt.Errorf("%w: %q", ErrDomain, d)
	}
	rest := d
	if strings.HasPrefix(d, "*.") {
		rest = d[2:]
	}
	labels := strings.Split(rest, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: %q", ErrDomain, d)
	}
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("%w: %q", ErrDomain, d)
		}
	}
	return nil
}

// ValidatePath 校验路由路径：必须以 / 开头，解码前后都不得含穿越、控制字符或配置语法字符。
func ValidatePath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 {
		return fmt.Errorf("%w: %q", ErrPath, p)
	}
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrPath, p)
	}
	for _, s := range []string{p, decoded} {
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: control character", ErrPath)
			}
		}
		if strings.ContainsAny(s, ";{}'\"`\\$#") {
			return fmt.Errorf("%w: forbidden character in %q", ErrPath, p)
		}
		for _, seg := range strings.Split(s, "/") {
			if seg == ".." || seg == "." {
				return fmt.Errorf("%w: traversal in %q", ErrPath, p)
			}
		}
	}
	if strings.Contains(strings.ToLower(p), "%2e") || strings.Contains(strings.ToLower(p), "%2f") {
		return fmt.Errorf("%w: encoded dot or slash in %q", ErrPath, p)
	}
	return nil
}

// 永远禁止的回源目标：回环、链路本地（含云元数据 169.254.169.254）、未指定地址。
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
}

// ValidateUpstream 校验 upstream 地址 host:port：必须是 IP 字面量且落在节点组登记的允许网段内。
// 域名形式的回源在 P1 中由控制台解析后再走本函数，并在 agent 侧二次校验。
func ValidateUpstream(hostport string, allowed []netip.Prefix) error {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		return fmt.Errorf("%w: %q is not ip:port", ErrUpstream, hostport)
	}
	addr := ap.Addr().Unmap()
	if ap.Port() == 0 {
		return fmt.Errorf("%w: port 0", ErrUpstream)
	}
	for _, d := range deniedPrefixes {
		if d.Contains(addr) {
			return fmt.Errorf("%w: %s is in denied range %s", ErrUpstream, addr, d)
		}
	}
	for _, a := range allowed {
		if a.Contains(addr) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside registered ranges", ErrUpstream, addr)
}
