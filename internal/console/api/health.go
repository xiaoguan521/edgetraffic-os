package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func (s *Server) leader() bool { return s.HA == nil || s.HA.IsLeader() }

func (s *Server) roleName() string {
	if s.leader() {
		return "leader"
	}
	return "standby"
}

func (s *Server) dbPing(ctx context.Context) bool {
	if s.Store == nil || s.Store.DB == nil {
		return false
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	db, err := s.Store.DB.DB()
	return err == nil && db.PingContext(c) == nil
}

// healthz 供 keepalived / 负载均衡探测（无需登录，不含敏感信息）：
// 主控制台且数据库可用时返回 200；备控制台或数据库不可用时返回 503。
func (s *Server) healthz(c *gin.Context) {
	out := gin.H{"version": version.Version, "role": "leader", "ha": s.HA != nil && s.HA.Enabled()}
	code := http.StatusOK
	if !s.leader() {
		out["role"] = "standby"
		code = http.StatusServiceUnavailable
	} else if s.HA != nil && !s.HA.Since().IsZero() {
		out["leader_since"] = s.HA.Since().UTC().Format(time.RFC3339)
	}
	if !s.dbPing(c.Request.Context()) {
		out["db"] = "unavailable"
		code = http.StatusServiceUnavailable
	} else {
		out["db"] = "ok"
	}
	out["status"] = map[bool]string{true: "ok", false: "unavailable"}[code == http.StatusOK]
	c.Header("Cache-Control", "no-store")
	c.JSON(code, out)
}

// livez 供进程存活探针使用：只要进程响应即返回 200（主备均返回 200）。
func (s *Server) livez(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"status":  "alive",
		"version": version.Version,
		"role":    s.roleName(),
	})
}

// readyz 供实例就绪探针使用：检查本实例数据库连接与基础就绪状态（备机就绪也返回 200，与 /healthz 的 VIP 选主语义区分）。
func (s *Server) readyz(c *gin.Context) {
	dbOK := s.dbPing(c.Request.Context())
	code := http.StatusOK
	status := "ready"
	dbStatus := "ok"
	if !dbOK {
		code = http.StatusServiceUnavailable
		status = "not_ready"
		dbStatus = "unavailable"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(code, gin.H{
		"status":  status,
		"version": version.Version,
		"role":    s.roleName(),
		"ha":      s.HA != nil && s.HA.Enabled(),
		"db":      dbStatus,
	})
}

func isLoopbackRemote(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	return err == nil && addr.Unmap().IsLoopback()
}

func promEscape(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// promMetrics 输出 Prometheus 文本格式指标（本机 loopback 免密采集，外部需 Bearer API Token 或会话认证）。
func (s *Server) promMetrics(c *gin.Context) {
	if !isLoopbackRemote(c.Request.RemoteAddr) {
		s.auth(c)
		if c.IsAborted() {
			return
		}
	}
	var b strings.Builder
	leaderVal := 0
	if s.leader() {
		leaderVal = 1
	}
	dbOK := s.dbPing(c.Request.Context())
	dbVal := 0
	if dbOK {
		dbVal = 1
	}
	licWritable := 1
	if s.Lic != nil && !s.Lic.Snapshot().Writable() {
		licWritable = 0
	}
	fmt.Fprintf(&b, "# HELP etos_console_leader Whether this console instance is the active leader.\n")
	fmt.Fprintf(&b, "# TYPE etos_console_leader gauge\n")
	fmt.Fprintf(&b, "etos_console_leader %d\n", leaderVal)
	fmt.Fprintf(&b, "# HELP etos_console_db_up Whether the console database is reachable.\n")
	fmt.Fprintf(&b, "# TYPE etos_console_db_up gauge\n")
	fmt.Fprintf(&b, "etos_console_db_up %d\n", dbVal)
	fmt.Fprintf(&b, "# HELP etos_license_writable Whether the license permits regular configuration writes.\n")
	fmt.Fprintf(&b, "# TYPE etos_license_writable gauge\n")
	fmt.Fprintf(&b, "etos_license_writable %d\n", licWritable)

	if dbOK {
		q := s.q()
		ctx := c.Request.Context()
		nodes, _ := q.Node.WithContext(ctx).Find()
		statusCount := map[string]int{"online": 0, "offline": 0, "revoked": 0}
		driftedCount := 0
		for _, n := range nodes {
			statusCount[n.Status]++
			if n.Drifted && n.Status != "revoked" {
				driftedCount++
			}
		}
		fmt.Fprintf(&b, "# HELP etos_nodes_total Number of nodes by status.\n")
		fmt.Fprintf(&b, "# TYPE etos_nodes_total gauge\n")
		for _, st := range []string{"online", "offline", "revoked"} {
			fmt.Fprintf(&b, "etos_nodes_total{status=%q} %d\n", st, statusCount[st])
		}
		fmt.Fprintf(&b, "# HELP etos_nodes_drifted_total Number of non-revoked nodes with configuration drift.\n")
		fmt.Fprintf(&b, "# TYPE etos_nodes_drifted_total gauge\n")
		fmt.Fprintf(&b, "etos_nodes_drifted_total %d\n", driftedCount)

		fmt.Fprintf(&b, "# HELP etos_node_cpu_percent Reported CPU usage percent per node.\n")
		fmt.Fprintf(&b, "# TYPE etos_node_cpu_percent gauge\n")
		fmt.Fprintf(&b, "# HELP etos_node_memory_used_bytes Reported memory used in bytes per node.\n")
		fmt.Fprintf(&b, "# TYPE etos_node_memory_used_bytes gauge\n")
		fmt.Fprintf(&b, "# HELP etos_node_active_connections Reported active connections per node.\n")
		fmt.Fprintf(&b, "# TYPE etos_node_active_connections gauge\n")
		fmt.Fprintf(&b, "# HELP etos_node_requests_total Reported total requests per node.\n")
		fmt.Fprintf(&b, "# TYPE etos_node_requests_total counter\n")
		for _, n := range nodes {
			if n.Status == "revoked" {
				continue
			}
			lbl := fmt.Sprintf(`node=%q`, promEscape(n.Name))
			fmt.Fprintf(&b, "etos_node_cpu_percent{%s} %.2f\n", lbl, n.CPUPercent)
			fmt.Fprintf(&b, "etos_node_memory_used_bytes{%s} %d\n", lbl, n.MemUsedBytes)
			fmt.Fprintf(&b, "etos_node_active_connections{%s} %d\n", lbl, n.ActiveConns)
			fmt.Fprintf(&b, "etos_node_requests_total{%s} %d\n", lbl, n.RequestsTotal)
		}

		certs, _ := q.Certificate.WithContext(ctx).Find()
		fmt.Fprintf(&b, "# HELP etos_certificates_total Total managed certificates.\n")
		fmt.Fprintf(&b, "# TYPE etos_certificates_total gauge\n")
		fmt.Fprintf(&b, "etos_certificates_total %d\n", len(certs))
		if len(certs) > 0 {
			now := time.Now()
			minSec := certs[0].NotAfter.Sub(now).Seconds()
			for _, cert := range certs[1:] {
				if sec := cert.NotAfter.Sub(now).Seconds(); sec < minSec {
					minSec = sec
				}
			}
			fmt.Fprintf(&b, "# HELP etos_certificates_min_expiry_seconds Minimum remaining validity in seconds across certificates.\n")
			fmt.Fprintf(&b, "# TYPE etos_certificates_min_expiry_seconds gauge\n")
			fmt.Fprintf(&b, "etos_certificates_min_expiry_seconds %.0f\n", minSec)
		}

		ae := q.AlertEvent
		firing, _ := ae.WithContext(ctx).Where(ae.Status.Eq("firing")).Count()
		fmt.Fprintf(&b, "# HELP etos_alerts_firing_total Number of currently firing alerts.\n")
		fmt.Fprintf(&b, "# TYPE etos_alerts_firing_total gauge\n")
		fmt.Fprintf(&b, "etos_alerts_firing_total %d\n", firing)

		if tsStr, err := s.Store.GetKV("last_backup_at"); err == nil {
			if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
				fmt.Fprintf(&b, "# HELP etos_backup_last_success_timestamp_seconds Unix timestamp of last successful backup.\n")
				fmt.Fprintf(&b, "# TYPE etos_backup_last_success_timestamp_seconds gauge\n")
				fmt.Fprintf(&b, "etos_backup_last_success_timestamp_seconds %d\n", t.Unix())
			}
		}
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
}

// standbyGuard：备控制台不提供 Web 控制台与 API（后台任务与节点连接都在主控制台），避免在备机上做出不会下发的修改。
func (s *Server) standbyGuard(c *gin.Context) {
	if s.leader() {
		c.Next()
		return
	}
	c.Header("Retry-After", "5")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": "STANDBY", "message": "这是备用控制台，请通过 VIP 访问主控制台"})
}
