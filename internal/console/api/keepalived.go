package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

const maxGroupVIPs = 16

var (
	ifaceNameRe  = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,32}$`)
	vrrpPassRe   = regexp.MustCompile(`^[A-Za-z0-9._@#%+=:-]{1,8}$`)
	consoleURLRe = regexp.MustCompile(`^https?://[A-Za-z0-9.\[\]:-]+/[A-Za-z0-9._/-]*$`)
)

// parseVIPList 解析逗号分隔的 IP 列表并去重保序。
func parseVIPList(csv string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(csv, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			continue
		}
		s := addr.Unmap().String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// normalizeVIPs 校验并规范化节点组登记的入口 VIP（逗号分隔的合法 IPv4/IPv6 地址，最多 16 个）。
func normalizeVIPs(csv string) (string, error) {
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(csv, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return "", fmt.Errorf("入口 VIP %q 格式无效（请填写单个 IPv4 或 IPv6 地址，不含掩码）", raw)
		}
		addr = addr.Unmap()
		if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
			return "", fmt.Errorf("入口 VIP %q 不能是回环、未指定或组播地址", raw)
		}
		s := addr.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) > maxGroupVIPs {
			return "", fmt.Errorf("单个节点组最多登记 %d 个入口 VIP", maxGroupVIPs)
		}
	}
	return strings.Join(out, ","), nil
}

// heldVIPs 计算节点当前上报网卡 IP 与节点组 VIP 的交集。
func heldVIPs(localIPsCSV, groupVIPsCSV string) []string {
	vips := parseVIPList(groupVIPsCSV)
	if len(vips) == 0 || localIPsCSV == "" {
		return nil
	}
	local := map[string]bool{}
	for _, s := range strings.Split(localIPsCSV, ",") {
		if ip := strings.TrimSpace(s); ip != "" {
			local[ip] = true
		}
	}
	var held []string
	for _, vip := range vips {
		if local[vip] {
			held = append(held, vip)
		}
	}
	return held
}

type keepalivedNodeInput struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Priority int    `json:"priority"`
}

type keepalivedRenderReq struct {
	Mode       string                `json:"mode"`        // edge | console
	GroupID    uint                  `json:"group_id"`    // edge 模式可选，自动带入该组状态页端口
	VIP        string                `json:"vip"`         // IP 或 IP/prefix，例如 10.0.0.100 或 10.0.0.100/24
	Interface  string                `json:"interface"`   // 网卡名，默认 eth0
	VRID       int                   `json:"vrid"`        // 1..255，默认 51
	AuthPass   string                `json:"auth_pass"`   // VRRP 口令（1..8 字符）
	ConsoleURL string                `json:"console_url"` // console 模式健康检查地址
	Nodes      []keepalivedNodeInput `json:"nodes"`       // 2..5 台节点（单播对端）
}

type keepalivedNodeConfig struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Priority int    `json:"priority"`
	Config   string `json:"config"`
}

type keepalivedRenderResp struct {
	Mode            string                 `json:"mode"`
	VIP             string                 `json:"vip"`
	CheckScriptPath string                 `json:"check_script_path"`
	CheckScript     string                 `json:"check_script"`
	Configs         []keepalivedNodeConfig `json:"configs"`
	InstallHint     string                 `json:"install_hint"`
}

func parseVIPWithMask(raw string) (netip.Addr, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, "", errors.New("请填写虚拟 IP（VIP）")
	}
	if strings.Contains(raw, "/") {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return netip.Addr{}, "", fmt.Errorf("VIP %q 格式无效", raw)
		}
		addr := p.Addr().Unmap()
		if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
			return netip.Addr{}, "", fmt.Errorf("VIP %q 不能是回环、未指定或组播地址", raw)
		}
		return addr, fmt.Sprintf("%s/%d", addr.String(), p.Bits()), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("VIP %q 格式无效", raw)
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
		return netip.Addr{}, "", fmt.Errorf("VIP %q 不能是回环、未指定或组播地址", raw)
	}
	bits := 24
	if addr.Is6() {
		bits = 64
	}
	return addr, fmt.Sprintf("%s/%d", addr.String(), bits), nil
}

// renderKeepalived 生成标准单播 Keepalived 配置与探活脚本（支持边缘节点入口 HA 与控制台主备 HA 两种场景）。
func (s *Server) renderKeepalived(c *gin.Context) {
	var req keepalivedRenderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "edge"
	}
	if mode != "edge" && mode != "console" {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "mode 仅支持 edge 或 console")
		return
	}
	vipAddr, vipCIDR, err := parseVIPWithMask(req.VIP)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	iface := strings.TrimSpace(req.Interface)
	if iface == "" {
		iface = "eth0"
	}
	if !ifaceNameRe.MatchString(iface) {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "网卡名格式无效（如 eth0、ens192、bond0）")
		return
	}
	vrid := req.VRID
	if vrid == 0 {
		vrid = 51
	}
	if vrid < 1 || vrid > 255 {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "virtual_router_id 必须在 1–255 之间")
		return
	}
	pass := strings.TrimSpace(req.AuthPass)
	if pass == "" {
		pass = "syha2026"
	}
	if !vrrpPassRe.MatchString(pass) {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "VRRP 认证口令需为 1–8 位字母、数字或常用符号（不含空格）")
		return
	}
	if len(req.Nodes) < 2 || len(req.Nodes) > 5 {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "高可用集群至少需要 2 台、最多 5 台节点")
		return
	}

	type peerNode struct {
		name     string
		ip       string
		priority int
	}
	peers := make([]peerNode, 0, len(req.Nodes))
	seenIP := map[string]bool{}
	for i, n := range req.Nodes {
		addr, err := netip.ParseAddr(strings.TrimSpace(n.IP))
		if err != nil {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("第 %d 台节点 IP %q 格式无效", i+1, n.IP))
			return
		}
		addr = addr.Unmap()
		if addr.Is6() != vipAddr.Is6() {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("第 %d 台节点 IP %s 与 VIP %s 地址族不一致（须同为 IPv4 或同为 IPv6）", i+1, addr.String(), vipAddr.String()))
			return
		}
		if addr == vipAddr {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("第 %d 台节点主机通信 IP 不能与入口 VIP %s 相同", i+1, vipAddr.String()))
			return
		}
		ip := addr.String()
		if seenIP[ip] {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("节点 IP %s 重复", ip))
			return
		}
		seenIP[ip] = true
		name := strings.TrimSpace(n.Name)
		if name == "" {
			name = fmt.Sprintf("node-%d", i+1)
		}
		if len(name) > 64 || strings.ContainsAny(name, "\r\n\t") {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("第 %d 台节点名称无效（不能包含换行或超过 64 字符）", i+1))
			return
		}
		prio := n.Priority
		if prio <= 0 {
			prio = 100 - i*10
			if prio < 10 {
				prio = 10
			}
		}
		if prio < 1 || prio > 254 {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("节点 %s 优先级须在 1–254 之间", name))
			return
		}
		peers = append(peers, peerNode{name: name, ip: ip, priority: prio})
	}

	var (
		scriptName  string
		scriptPath  string
		checkScript string
		instanceTag string
		probeHint   string
	)
	if mode == "console" {
		consoleURL := strings.TrimSpace(req.ConsoleURL)
		if consoleURL == "" {
			consoleURL = "https://127.0.0.1:8443/healthz"
		}
		if !consoleURLRe.MatchString(consoleURL) {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "控制台健康检查地址格式无效（示例：https://127.0.0.1:8443/healthz）")
			return
		}
		scriptName = "chk_" + brand.ID() + "_console"
		scriptPath = "/usr/local/bin/chk-" + brand.ID() + "-console.sh"
		instanceTag = strings.ToUpper(brand.ID()) + "_CONSOLE"
		checkScript = fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
# 主控制台 /healthz 返回 200，备控制台返回 503，VIP 自动跟随主控制台
exec /usr/bin/curl -sfk -o /dev/null --max-time 2 "%s"
`, consoleURL)
		probeHint = "控制台主备依赖共用 PostgreSQL 选主，keepalived 每 2 秒探测 /healthz（200/503）自动漂移 VIP。"
	} else {
		prof := render.Profile{}
		if req.GroupID > 0 && s.Deploy != nil {
			if p, perr := s.Deploy.Profile(req.GroupID); perr == nil {
				prof = p
			}
		}
		statusURL := fmt.Sprintf("http://127.0.0.1:%d%s", prof.Status(), layout.StatusPath())
		instSlug := strings.TrimSpace(prof.Instance)
		if instSlug != "" {
			safeUnderscore := strings.ReplaceAll(instSlug, "-", "_")
			scriptName = "chk_" + brand.ID() + "_edge_" + safeUnderscore
			scriptPath = "/usr/local/bin/chk-" + brand.ID() + "-edge-" + instSlug + ".sh"
			instanceTag = strings.ToUpper(brand.ID() + "_EDGE_" + safeUnderscore)
		} else {
			scriptName = "chk_" + brand.ID() + "_edge"
			scriptPath = "/usr/local/bin/chk-" + brand.ID() + "-edge.sh"
			instanceTag = strings.ToUpper(brand.ID()) + "_EDGE"
		}
		checkScript = fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
# 探测本实例专属的本机状态页（同时验证 %s 进程与 worker 正常响应，多实例互不干扰）
exec /usr/bin/curl -sf -o /dev/null --max-time 2 "%s"
`, brand.Proc(), statusURL)
		probeHint = fmt.Sprintf("探活脚本每 2 秒检测本实例状态页（%s），异常连续 2 次后自动降权漂移 VIP；Agent 心跳会只读感知 VIP 归属并在控制台标注主备角色。", statusURL)
	}

	// RFC 5798：IPv6 使用 VRRPv3，不支持 authentication { auth_type PASS }；仅 IPv4 输出认证块。
	authBlock := ""
	if !vipAddr.Is6() {
		authBlock = fmt.Sprintf(`    authentication {
        auth_type PASS
        auth_pass %s
    }
`, pass)
	}

	configs := make([]keepalivedNodeConfig, 0, len(peers))
	for i, self := range peers {
		var others []string
		for j, o := range peers {
			if i == j {
				continue
			}
			others = append(others, "        "+o.ip)
		}
		cfg := fmt.Sprintf(`# %s (%s) — /etc/keepalived/keepalived.conf
# 由 %s 控制台生成（单播 VRRP 模式，兼容物理机、虚拟机与云内网）
global_defs {
    router_id %s_%d
    enable_script_security
    script_user root
}

vrrp_script %s {
    script "%s"
    interval 2
    timeout 2
    fall 2
    rise 1
    weight -50
}

vrrp_instance %s {
    state BACKUP
    nopreempt
    interface %s
    virtual_router_id %d
    priority %d
    advert_int 1
    unicast_src_ip %s
    unicast_peer {
%s
    }
%s    virtual_ipaddress {
        %s
    }
    track_script {
        %s
    }
}
`, self.name, self.ip, brand.Product(), instanceTag, i+1, scriptName, scriptPath, instanceTag, iface, vrid, self.priority, self.ip, strings.Join(others, "\n"), authBlock, vipCIDR, scriptName)
		configs = append(configs, keepalivedNodeConfig{
			Name:     self.name,
			IP:       self.ip,
			Priority: self.priority,
			Config:   cfg,
		})
	}

	installHint := fmt.Sprintf(`1. 在每台参与高可用的主机安装 keepalived（如 apt install -y keepalived 或 dnf install -y keepalived）。
2. 将探活脚本写入 %s 并执行 chmod 0755 %s。
3. 将对应主机的配置写入 /etc/keepalived/keepalived.conf，执行 systemctl enable --now keepalived。
4. %s`, scriptPath, scriptPath, probeHint)

	c.JSON(http.StatusOK, keepalivedRenderResp{
		Mode:            mode,
		VIP:             vipCIDR,
		CheckScriptPath: scriptPath,
		CheckScript:     checkScript,
		Configs:         configs,
		InstallHint:     installHint,
	})
}
