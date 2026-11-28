// Package api 是控制台的 REST API（规格书 7.1、10.2，MVP 子集）。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/acme"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/alerts"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/ha"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/secpolicy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// Server 汇总 API 依赖。
type Server struct {
	Store         *store.Store
	GW            *gateway.Server
	Deploy        *deploy.Service
	Certs         *certs.Service
	Acme          *acme.Service
	Alerts        *alerts.Service
	Lic           *licsvc.Service
	Audit         *audit.Logger
	Log           *slog.Logger
	Web           fs.FS  // 前端构建产物（web/dist）
	GRPCAddr      string // 节点连接控制台使用的地址，写入安装命令
	CAFingerprint string
	SecureCookie  bool
	WebCAPEM      []byte      // -tls-auto 时的 Web CA 证书，公开下载以便导入浏览器
	HA            *ha.Elector // 主备选主；nil 视为单机

	jwtKey    []byte
	fails     sync.Map // ip -> *loginFail
	ruleTplMu sync.Mutex
	routes    func() gin.RoutesInfo
}

type loginFail struct {
	mu    sync.Mutex
	n     int
	until time.Time
}

const cookieName = "etos_session"

type auditE = audit.Entry

// Handler 构造 HTTP 路由。
func (s *Server) Handler() (http.Handler, error) {
	key, err := s.Store.GetKV("jwt_secret")
	if err != nil {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		key = hex.EncodeToString(b)
		if err := s.Store.SetKV("jwt_secret", key); err != nil {
			return nil, err
		}
	}
	s.jwtKey = []byte(key)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), s.securityHeaders)
	r.GET("/healthz", s.healthz)
	r.GET("/livez", s.livez)
	r.GET("/readyz", s.readyz)
	r.GET("/metrics", s.promMetrics)
	r.Use(s.standbyGuard)

	if len(s.WebCAPEM) > 0 {
		r.GET("/web-ca.crt", func(c *gin.Context) {
			c.Header("Content-Disposition", `attachment; filename="edgetraffic-console-web-ca.crt"`)
			c.Data(http.StatusOK, "application/x-x509-ca-cert", s.WebCAPEM)
		})
	}

	v1 := r.Group("/api/v1")
	v1.Use(s.csrf)
	v1.GET("/brand", s.brandInfo)
	v1.GET("/setup/status", s.setupStatus)
	v1.POST("/setup", s.setup)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/logout", s.logout)
	v1.GET("/auth/oidc/start", s.oidcStart)
	v1.GET("/auth/oidc/callback", s.oidcCallback)

	a := v1.Group("", s.auth)
	a.GET("/auth/me", s.me)
	a.GET("/openapi.json", s.openAPI)

	// 告警（P9，规格书 7.1 M9）：企业微信 / 钉钉通道需要授权功能 alert_im
	a.GET("/alerts", s.alertsGet)
	a.PUT("/alerts/silence", role(store.RoleAdmin), s.writable, s.setAlertSilence)
	a.POST("/alert-channels", role(store.RoleAdmin), s.writable, s.saveChannel)
	a.PUT("/alert-channels/:id", role(store.RoleAdmin), s.writable, s.saveChannel)
	a.DELETE("/alert-channels/:id", role(store.RoleAdmin), s.deleteChannel)
	a.POST("/alert-channels/:id/test", role(store.RoleAdmin), s.testChannel)
	a.PUT("/alert-rules/:kind", role(store.RoleAdmin), s.writable, s.saveRule)

	// 开放 API 令牌（P9）：创建需要授权功能 api_token；撤销任何时候都可以；令牌不能再创建令牌
	a.GET("/api-tokens", s.listTokens)
	a.POST("/api-tokens", s.writable, s.feature(license.FeatureAPIToken), s.createToken)
	a.DELETE("/api-tokens/:id", s.revokeToken)
	a.GET("/dashboard", s.dashboard)
	a.GET("/badges", s.badges)
	a.GET("/search", s.globalSearch)
	a.GET("/metrics", s.metrics)

	a.GET("/license", s.licenseGet)
	a.GET("/license/request", role(store.RoleAdmin), s.licenseRequest)
	a.POST("/license/import", role(store.RoleAdmin), s.licenseImport) // 过期状态下仍可用
	a.POST("/license/crl", role(store.RoleAdmin), s.licenseCRL)       // 过期状态下仍可用

	a.GET("/node-groups", s.listGroups)
	a.POST("/node-groups", role(store.RoleAdmin), s.writable, s.createGroup)
	a.PUT("/node-groups/:id", role(store.RoleOperator), s.writable, s.updateGroup)
	a.GET("/node-groups/:id/usage", s.groupUsage)
	a.DELETE("/node-groups/:id", role(store.RoleAdmin), s.writable, s.deleteGroup)
	a.GET("/node-groups/:id/profile", s.getProfile)
	a.PUT("/node-groups/:id/profile", role(store.RoleOperator), s.writable, s.setProfile)
	a.GET("/node-groups/:id/management-mode", s.getManagementMode)
	a.PUT("/node-groups/:id/management-mode", role(store.RoleOperator), s.writable, s.setManagementMode)
	a.POST("/node-groups/:id/copy-from", role(store.RoleOperator), s.writable, s.copyGroupConfig)
	a.POST("/node-groups/:id/enroll", role(store.RoleOperator), s.writable, s.enrollToken)
	a.GET("/node-groups/:id/preview", s.preview)
	a.POST("/node-groups/:id/publish", role(store.RoleReleaser), s.writable, s.publish)
	a.POST("/node-groups/:id/rollback", role(store.RoleReleaser), s.rollback) // 过期状态下仍可用
	a.POST("/node-groups/:id/release-requests", role(store.RoleReleaser), s.writable, s.requestRelease)
	a.POST("/node-groups/:id/legacy-requests", role(store.RoleAdmin), s.writable, s.feature(license.FeatureLua), s.requestLegacy)
	// 回退申请不挂 writable / feature：EXPIRED / INVALID 下回滚必须可用；仍需管理员提交、另一位管理员批准。
	a.POST("/node-groups/:id/legacy-rollback-requests", role(store.RoleAdmin), s.requestLegacyRollback)
	a.POST("/node-groups/:id/canary", role(store.RoleReleaser), s.writable, s.feature(license.FeatureCanary), s.startCanary)
	a.GET("/canaries", s.listCanaries)
	a.POST("/releases/batch", role(store.RoleReleaser), s.writable, s.feature(license.FeatureBatchRelease), s.batchPublish)

	// 数据面版本库与分批热升级（P8，规格书 12.6）：全程不中断连接，任一节点失败即停止
	a.GET("/packages", role(store.RoleOperator), s.listPackages)
	a.POST("/packages", role(store.RoleAdmin), s.writable, s.importPackage)
	a.POST("/node-groups/:id/upgrade", role(store.RoleOperator), s.writable, s.startUpgrade)
	a.POST("/node-groups/:id/agent-upgrade", role(store.RoleOperator), s.writable, s.startAgentUpgrade)
	a.GET("/upgrades", s.listUpgrades)
	a.GET("/upgrades/:id", s.getUpgrade)
	// 同机多实例（设计 0001 第 5.6 节）：主机部分升级后重试剩余实例，或把已升级的实例整体回退
	a.POST("/upgrades/:id/retry", role(store.RoleOperator), s.writable, s.retryUpgrade)
	a.POST("/upgrades/:id/rollback-hosts", role(store.RoleOperator), s.writable, s.rollbackHosts)
	a.GET("/hosts", s.listHosts)

	// 配置导入（P7，规格书 12.4）
	a.GET("/imports", role(store.RoleOperator), s.listImports)
	a.POST("/imports", role(store.RoleOperator), s.writable, s.createImport)
	a.GET("/imports/:id", role(store.RoleOperator), s.getImport)
	a.GET("/imports/:id/report", role(store.RoleOperator), s.downloadImport)
	a.GET("/imports/:id/review", role(store.RoleOperator), s.reviewImport)
	a.POST("/imports/:id/apply", role(store.RoleOperator), s.writable, s.applyImport)

	// 自定义片段（P7，规格书 12.7）：每次变更都需另一人批准
	a.GET("/snippets", s.listSnippets)
	a.GET("/snippets/stats", role(store.RoleOperator), s.snippetStats)
	a.POST("/snippets/validate", role(store.RoleOperator), s.validateSnippet)
	a.POST("/snippets", role(store.RoleOperator), s.writable, s.submitSnippet)
	a.POST("/snippets/:id/delete", role(store.RoleOperator), s.writable, s.deleteSnippet)
	a.POST("/snippets/:id/approve", role(store.RoleOperator), s.writable, s.approveSnippet)
	a.POST("/snippets/:id/reject", role(store.RoleOperator), s.rejectSnippet)
	a.POST("/snippets/:id/withdraw", role(store.RoleOperator), s.withdrawSnippet)

	// 发布审批（P7）：开启需要 release_approval 功能，关闭任何时候都可以
	a.GET("/release-approval", s.approvalGet)
	a.PUT("/release-approval", role(store.RoleAdmin), s.approvalPut)
	a.GET("/release-requests", s.listReleaseRequests)
	a.GET("/release-requests/:id", s.getReleaseRequest)
	a.POST("/release-requests/:id/approve", role(store.RoleReleaser), s.writable, s.approveRelease)
	// 批准同时用于新包与回退：授权检查在 approveLegacy 内按请求类型进行（新包要求可写与 lua 授权，回退不要求）。
	a.POST("/release-requests/:id/legacy-approve", role(store.RoleAdmin), s.approveLegacy)
	a.GET("/release-requests/:id/legacy-bundle", role(store.RoleAdmin), s.reviewLegacyBundle)
	a.POST("/release-requests/:id/reject", role(store.RoleReleaser), s.rejectRelease)
	a.POST("/release-requests/:id/cancel", role(store.RoleReleaser), s.cancelRelease)

	a.GET("/nodes", s.listNodes)
	a.PATCH("/nodes/:id", role(store.RoleOperator), s.writable, s.updateNode)
	a.POST("/nodes/:id/sync", role(store.RoleReleaser), s.writable, s.syncNode)
	a.POST("/nodes/:id/revoke", role(store.RoleAdmin), s.revokeNode)
	a.POST("/nodes/:id/restore", role(store.RoleReleaser), s.restoreNode) // 漂移恢复：与回滚一样在过期状态下可用
	a.GET("/nodes/:id/drift", role(store.RoleOperator), s.nodeDrift)
	a.POST("/nodes/:id/drift/adopt", role(store.RoleOperator), s.writable, s.adoptDrift)

	a.GET("/sites", s.listSites)
	a.GET("/sites/:id", s.getSite)
	a.POST("/sites", role(store.RoleOperator), s.writable, s.createSite)
	a.PUT("/sites/:id", role(store.RoleOperator), s.writable, s.updateSite)
	a.DELETE("/sites/:id", role(store.RoleOperator), s.writable, s.deleteSite)

	a.GET("/ip-sets", s.listIPSets)
	a.GET("/ip-sets/:id", s.getIPSet)
	a.POST("/ip-sets", role(store.RoleOperator), s.writable, s.createIPSet)
	a.POST("/ip-sets/import", role(store.RoleOperator), s.writable, s.importIPSet)
	a.PUT("/ip-sets/:id", role(store.RoleOperator), s.writable, s.updateIPSet)
	a.DELETE("/ip-sets/:id", role(store.RoleOperator), s.writable, s.deleteIPSet)

	a.GET("/rule-templates", s.listRuleTemplates)
	a.POST("/rule-templates", role(store.RoleOperator), s.writable, s.saveRuleTemplate)
	a.DELETE("/rule-templates/:name", role(store.RoleOperator), s.writable, s.deleteRuleTemplate)

	a.GET("/certificates", s.listCerts)
	a.POST("/certificates", role(store.RoleOperator), s.writable, s.createCert)
	a.PUT("/certificates/:id", role(store.RoleOperator), s.renewCert)          // 证书续期：任何授权状态下可用（规格书 8.6）
	a.POST("/certificates/:id/deploy", role(store.RoleOperator), s.deployCert) // 重新下发证书：同上
	a.DELETE("/certificates/:id", role(store.RoleOperator), s.writable, s.deleteCert)
	a.POST("/certificates/acme", role(store.RoleOperator), s.writable, s.acmeIssue)
	a.POST("/certificates/gm", role(store.RoleOperator), s.writable, s.feature(license.FeatureGMTLS), s.createGMCert)
	a.PUT("/certificates/:id/gm", role(store.RoleOperator), s.renewGMCert)        // 国密证书续期：任何授权状态下可用
	a.POST("/certificates/:id/acme-renew", role(store.RoleOperator), s.acmeRenew) // ACME 续期：任何授权状态下可用
	a.GET("/acme", s.acmeGet)
	a.PUT("/acme", role(store.RoleAdmin), s.writable, s.acmePut)

	// 静态资源发布（P9）
	a.GET("/static-bundles", s.listStatic)
	a.POST("/static-bundles", role(store.RoleOperator), s.writable, s.uploadStatic)
	a.DELETE("/static-bundles/:id", role(store.RoleOperator), s.deleteStatic)

	// 四层代理（P9）：新增与修改需要授权功能 stream；删除任何时候都可以（授权失效时可移除后继续发布）
	a.GET("/streams", s.listStreams)
	a.POST("/streams", role(store.RoleOperator), s.writable, s.feature(license.FeatureStream), s.saveStream)
	a.PUT("/streams/:id", role(store.RoleOperator), s.writable, s.feature(license.FeatureStream), s.saveStream)
	a.DELETE("/streams/:id", role(store.RoleOperator), s.deleteStream)

	a.GET("/upstreams", s.listUpstreams)
	a.POST("/upstreams", role(store.RoleOperator), s.writable, s.createUpstream)
	a.PUT("/upstreams/:id", role(store.RoleOperator), s.writable, s.updateUpstream)
	a.DELETE("/upstreams/:id", role(store.RoleOperator), s.writable, s.deleteUpstream)

	a.GET("/revisions", s.listRevisions)
	a.GET("/revisions/diff", s.diffRevisions)
	a.GET("/revisions/:id/files", s.revisionFiles)
	a.GET("/deployments", s.listDeployments)
	a.GET("/deployments/:id", s.getDeployment)

	a.GET("/audit", role(store.RoleViewer), s.listAudit)
	a.GET("/audit/verify", role(store.RoleAdmin), s.verifyAudit)
	a.GET("/audit/export", role(store.RoleAdmin), s.feature(license.FeatureAuditExport), s.exportAudit)

	a.PUT("/auth/password", s.changePassword)
	a.GET("/sso/oidc", role(store.RoleAdmin), s.getOIDC)
	a.PUT("/sso/oidc", role(store.RoleAdmin), s.putOIDC)
	a.GET("/sso/ldap", role(store.RoleAdmin), s.getLDAP)
	a.PUT("/sso/ldap", role(store.RoleAdmin), s.putLDAP)
	a.POST("/sso/ldap/test", role(store.RoleAdmin), s.testLDAP)
	a.GET("/security-policy", role(store.RoleAdmin), s.getPolicy)
	a.PUT("/security-policy", role(store.RoleAdmin), s.putPolicy)
	a.PUT("/users/:id", role(store.RoleAdmin), s.updateUser)
	a.DELETE("/users/:id", role(store.RoleAdmin), s.deleteUser)
	a.GET("/users", role(store.RoleAdmin), s.listUsers)
	a.POST("/users", role(store.RoleAdmin), s.writable, s.createUser)

	// 前端：静态文件 + SPA 回退到 index.html
	if s.Web != nil {
		files := http.FileServer(http.FS(s.Web))
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				fail(c, http.StatusNotFound, "NOT_FOUND", "not found")
				return
			}
			p := strings.TrimPrefix(c.Request.URL.Path, "/")
			if p == "" || !exists(s.Web, p) {
				c.Request.URL.Path = "/"
			}
			files.ServeHTTP(c.Writer, c.Request)
		})
	}
	s.routes = r.Routes
	return r, nil
}

func exists(f fs.FS, p string) bool {
	st, err := fs.Stat(f, p)
	return err == nil && !st.IsDir()
}

func (s *Server) securityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; script-src 'self'")
	c.Next()
}

// csrf：修改类请求必须带自定义头（浏览器跨站表单无法设置），配合 SameSite=Strict Cookie。
func (s *Server) csrf(c *gin.Context) {
	// 令牌认证不依赖浏览器自动携带的凭据，不受 CSRF 影响
	if strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
		c.Next()
		return
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.GetHeader("X-Requested-With") != "etos" {
		fail(c, http.StatusForbidden, "CSRF", "missing X-Requested-With header")
		return
	}
	c.Next()
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, apiError{Code: code, Message: msg})
}

func failErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, deploy.ErrGroupHasNodes):
		fail(c, http.StatusConflict, "GROUP_HAS_NODES", err.Error())
	case errors.Is(err, deploy.ErrGroupBusy):
		fail(c, http.StatusConflict, "GROUP_BUSY", err.Error())
	case errors.Is(err, deploy.ErrLastGroup):
		fail(c, http.StatusConflict, "LAST_GROUP", err.Error())
	case errors.Is(err, deploy.ErrBusy):
		fail(c, http.StatusConflict, "DEPLOY_BUSY", err.Error())
	case errors.Is(err, deploy.ErrNoChanges):
		fail(c, http.StatusConflict, "NO_CHANGES", err.Error())
	case errors.Is(err, deploy.ErrConfigManagementPaused):
		fail(c, http.StatusConflict, "CONFIG_MANAGEMENT_PAUSED", err.Error())
	case errors.Is(err, deploy.ErrHandoffPending):
		fail(c, http.StatusConflict, "CONFIG_HANDOFF_PENDING", err.Error())
	case errors.Is(err, deploy.ErrLegacyAdminRequired):
		fail(c, http.StatusForbidden, "LEGACY_ADMIN_REQUIRED", "原生兼容配置必须由另一位管理员审批")
	case errors.Is(err, deploy.ErrLegacyModeRequired):
		fail(c, http.StatusConflict, "LEGACY_MODE_REQUIRED", "该操作需要兼容配置模式和兼容版本")
	case errors.Is(err, deploy.ErrInvalidManagementMode):
		fail(c, http.StatusBadRequest, "INVALID_MANAGEMENT_MODE", err.Error())
	case errors.Is(err, deploy.ErrApprovalRequired):
		fail(c, http.StatusConflict, "APPROVAL_REQUIRED", "已开启发布审批：请提交发布申请，由另一位发布员批准后下发")
	case errors.Is(err, deploy.ErrPendingRequest):
		fail(c, http.StatusConflict, "PENDING_REQUEST", "该节点组已有待审批的发布申请")
	case errors.Is(err, deploy.ErrRequestClosed):
		fail(c, http.StatusConflict, "REQUEST_CLOSED", "该申请已处理")
	case errors.Is(err, deploy.ErrStaleRequest):
		fail(c, http.StatusConflict, "STALE_REQUEST", "提交后线上版本已变化（例如有人回滚），该申请已过期，请重新提交")
	case errors.Is(err, deploy.ErrSelfApproval):
		fail(c, http.StatusForbidden, "SELF_APPROVAL", "提交人不能审批自己的发布申请")
	case errors.Is(err, deploy.ErrCanaryRunning):
		fail(c, http.StatusConflict, "CANARY_RUNNING", "该节点组正在灰度发布")
	case errors.Is(err, deploy.ErrStreamNotLicensed):
		fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", err.Error())
	case errors.Is(err, deploy.ErrUpgradeRunning):
		fail(c, http.StatusConflict, "UPGRADE_RUNNING", "该节点组正在升级")
	case errors.Is(err, deploy.ErrNoDriftReport):
		fail(c, http.StatusNotFound, "NO_DRIFT_REPORT", err.Error())
	case errors.Is(err, deploy.ErrLuaScriptNotLicensed):
		fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", err.Error())
	case errors.Is(err, deploy.ErrNoPendingChange):
		fail(c, http.StatusConflict, "NO_PENDING_CHANGE", "该片段没有待审批的变更")
	case errors.Is(err, deploy.ErrForbidden):
		fail(c, http.StatusForbidden, "FORBIDDEN", "只有提交人或管理员可以撤回")
	default:
		fail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	}
}

// ---------- 认证与授权 ----------

type claims struct {
	UID    uint   `json:"uid"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	AuthAt int64  `json:"auth_at"` // 登录时间：会话最长有效期从这里算
	PC     bool   `json:"pc"`      // 必须先修改口令（首次登录或口令过期）
	TV     int    `json:"tv"`      // 签发时的 User.TokenVersion；与数据库不一致即已被撤销
	jwt.RegisteredClaims
}

// issue 签发会话：空闲超时后失效，活动时滑动续期，自登录起最长 MaxHours（等保 2.0 三级）。
func (s *Server) issue(c *gin.Context, u *store.User) error {
	return s.issueAt(c, u, time.Now().Unix(), u.MustChangePassword || (isLocal(u) && s.policy().Expired(u.PasswordChangedAt, u.CreatedAt, time.Now())))
}

func (s *Server) issueAt(c *gin.Context, u *store.User, authAt int64, pc bool) error {
	pol := s.policy()
	idle := time.Duration(pol.IdleMinutes) * time.Minute
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{UID: u.ID, Name: u.Username, Role: u.Role, AuthAt: authAt, PC: pc, TV: u.TokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(idle)), IssuedAt: jwt.NewNumericDate(time.Now())}})
	str, err := tok.SignedString(s.jwtKey)
	if err != nil {
		return err
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: str, Path: "/", HttpOnly: true, Secure: s.SecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: pol.MaxHours * 3600})
	return nil
}

func (s *Server) auth(c *gin.Context) {
	if strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
		s.tokenAuth(c)
		return
	}
	ck, err := c.Cookie(cookieName)
	if err != nil {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "login required")
		return
	}
	var cl claims
	tok, err := jwt.ParseWithClaims(ck, &cl, func(t *jwt.Token) (any, error) { return s.jwtKey, nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !tok.Valid {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "session expired")
		return
	}
	pol := s.policy()
	if time.Since(time.Unix(cl.AuthAt, 0)) > time.Duration(pol.MaxHours)*time.Hour {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "session expired, please sign in again")
		return
	}
	// 每次请求按数据库中的账号状态判断：停用、删除、改角色立即生效
	uq := s.q().User
	u, err := uq.WithContext(c.Request.Context()).Where(uq.ID.Eq(cl.UID)).First()
	if err != nil || u.Disabled || u.Username != cl.Name {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "account disabled or removed")
		return
	}
	if cl.TV != u.TokenVersion {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "session revoked, please sign in again")
		return
	}
	if cl.PC {
		p := strings.TrimPrefix(c.Request.URL.Path, "/api/v1")
		if p != "/auth/password" && p != "/auth/me" && p != "/auth/logout" {
			fail(c, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "请先修改口令")
			return
		}
	}
	// 滑动续期：剩余时间不足一半空闲超时时重新签发
	if cl.ExpiresAt != nil && time.Until(cl.ExpiresAt.Time) < time.Duration(pol.IdleMinutes)*time.Minute/2 {
		_ = s.issueAt(c, u, cl.AuthAt, cl.PC)
	}
	c.Set("uid", u.ID)
	c.Set("user", u.Username)
	c.Set("role", u.Role)
	c.Set("pc", cl.PC)
	c.Next()
}

func isLocal(u *store.User) bool { return u.Source == "" || u.Source == "local" }

// policy 返回当前账号安全策略。
func (s *Server) policy() secpolicy.Policy {
	v, _ := s.Store.GetKV("security_policy")
	return secpolicy.Parse(v)
}

var rank = map[string]int{store.RoleViewer: 1, store.RoleReleaser: 2, store.RoleOperator: 3, store.RoleAdmin: 4}

func role(min string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rank[c.GetString("role")] < rank[min] {
			fail(c, http.StatusForbidden, "FORBIDDEN", "requires role "+min)
			return
		}
		c.Next()
	}
}

// writable：授权过期 / 无效时拦截新增变更（规格书 8.6）。回滚、导入 License 不挂此中间件。
func (s *Server) writable(c *gin.Context) {
	snap := s.Lic.Snapshot()
	if !snap.Writable() {
		fail(c, http.StatusLocked, "LICENSE_READ_ONLY", "授权状态为 "+string(snap.State)+"，控制台只读："+snap.Reason)
		return
	}
	c.Next()
}

func (s *Server) feature(f license.Feature) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.Lic.Snapshot().Feature(f) {
			fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", "当前授权不包含功能 "+string(f))
			return
		}
		c.Next()
	}
}

func (s *Server) audit(c *gin.Context, action, objType, objID string, detail any) {
	_ = s.Audit.Write(audit.Entry{Username: c.GetString("user"), IP: c.ClientIP(), Action: action, ObjectType: objType, ObjectID: objID, Detail: detail})
}
