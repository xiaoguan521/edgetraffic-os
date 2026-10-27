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

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// Server 汇总 API 依赖。
type Server struct {
	Store         *store.Store
	GW            *gateway.Server
	Deploy        *deploy.Service
	Lic           *licsvc.Service
	Audit         *audit.Logger
	Log           *slog.Logger
	Web           fs.FS  // 前端构建产物（web/dist）
	GRPCAddr      string // 节点连接控制台使用的地址，写入安装命令
	CAFingerprint string
	SecureCookie  bool

	jwtKey []byte
	fails  sync.Map // ip -> *loginFail
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

	v1 := r.Group("/api/v1")
	v1.Use(s.csrf)
	v1.GET("/setup/status", s.setupStatus)
	v1.POST("/setup", s.setup)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/logout", s.logout)

	a := v1.Group("", s.auth)
	a.GET("/auth/me", s.me)
	a.GET("/dashboard", s.dashboard)
	a.GET("/metrics", s.metrics)

	a.GET("/license", s.licenseGet)
	a.GET("/license/request", role(store.RoleAdmin), s.licenseRequest)
	a.POST("/license/import", role(store.RoleAdmin), s.licenseImport) // 过期状态下仍可用

	a.GET("/node-groups", s.listGroups)
	a.POST("/node-groups", role(store.RoleAdmin), s.writable, s.createGroup)
	a.PUT("/node-groups/:id", role(store.RoleOperator), s.writable, s.updateGroup)
	a.POST("/node-groups/:id/enroll", role(store.RoleOperator), s.writable, s.enrollToken)
	a.GET("/node-groups/:id/preview", s.preview)
	a.POST("/node-groups/:id/publish", role(store.RoleReleaser), s.writable, s.publish)
	a.POST("/node-groups/:id/rollback", role(store.RoleReleaser), s.rollback) // 过期状态下仍可用

	a.GET("/nodes", s.listNodes)
	a.POST("/nodes/:id/revoke", role(store.RoleAdmin), s.revokeNode)

	a.GET("/sites", s.listSites)
	a.GET("/sites/:id", s.getSite)
	a.POST("/sites", role(store.RoleOperator), s.writable, s.createSite)
	a.PUT("/sites/:id", role(store.RoleOperator), s.writable, s.updateSite)
	a.DELETE("/sites/:id", role(store.RoleOperator), s.writable, s.deleteSite)

	a.GET("/upstreams", s.listUpstreams)
	a.POST("/upstreams", role(store.RoleOperator), s.writable, s.createUpstream)
	a.PUT("/upstreams/:id", role(store.RoleOperator), s.writable, s.updateUpstream)
	a.DELETE("/upstreams/:id", role(store.RoleOperator), s.writable, s.deleteUpstream)

	a.GET("/revisions", s.listRevisions)
	a.GET("/deployments", s.listDeployments)
	a.GET("/deployments/:id", s.getDeployment)

	a.GET("/audit", role(store.RoleViewer), s.listAudit)
	a.GET("/audit/verify", role(store.RoleAdmin), s.verifyAudit)
	a.GET("/audit/export", role(store.RoleAdmin), s.feature(license.FeatureAuditExport), s.exportAudit)

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
	case errors.Is(err, deploy.ErrBusy):
		fail(c, http.StatusConflict, "DEPLOY_BUSY", err.Error())
	case errors.Is(err, deploy.ErrNoChanges):
		fail(c, http.StatusConflict, "NO_CHANGES", err.Error())
	default:
		fail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	}
}

// ---------- 认证与授权 ----------

type claims struct {
	UID  uint   `json:"uid"`
	Name string `json:"name"`
	Role string `json:"role"`
	jwt.RegisteredClaims
}

func (s *Server) issue(c *gin.Context, u *store.User) error {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{UID: u.ID, Name: u.Username, Role: u.Role,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(8 * time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}})
	str, err := tok.SignedString(s.jwtKey)
	if err != nil {
		return err
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: str, Path: "/", HttpOnly: true, Secure: s.SecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	return nil
}

func (s *Server) auth(c *gin.Context) {
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
	c.Set("uid", cl.UID)
	c.Set("user", cl.Name)
	c.Set("role", cl.Role)
	c.Next()
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
