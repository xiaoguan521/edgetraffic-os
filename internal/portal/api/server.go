// Package api 是授权后台的 HTTP 接口（规格书 8.8）。业务规则在 internal/portal/service 中；
// 这里只做认证、参数绑定与错误映射。授权后台部署在厂商内网，只持有公钥。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/service"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/query"
)

// Server 是授权后台的 HTTP 服务。
type Server struct {
	Svc          *service.Service
	Store        *store.Store
	Log          *slog.Logger
	Web          fs.FS // 前端构建产物（web/portal/dist）
	SecureCookie bool

	jwtKey []byte
	fails  sync.Map // ip -> *loginFail
}

type loginFail struct {
	mu    sync.Mutex
	n     int
	until time.Time
}

const cookieName = "etos_portal_session"

// Handler 构造路由。
func (s *Server) Handler() (http.Handler, error) {
	key, err := s.secret(context.Background())
	if err != nil {
		return nil, err
	}
	s.jwtKey = key
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), securityHeaders)
	v1 := r.Group("/api/v1", csrf)
	v1.GET("/setup/status", s.setupStatus)
	v1.POST("/setup", s.setup)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/logout", s.logout)

	a := v1.Group("", s.auth)
	a.GET("/auth/me", s.me)
	a.GET("/dashboard", s.dashboard)
	a.GET("/customers", s.listCustomers)
	a.POST("/customers", s.createCustomer)
	a.PUT("/customers/:id", s.updateCustomer)
	a.GET("/orders", s.listOrders)
	a.POST("/orders", s.createOrder)
	a.GET("/licenses", s.listLicenses)
	a.POST("/licenses", s.createDraft)
	a.GET("/licenses/:id", s.getLicense)
	a.DELETE("/licenses/:id", s.deleteDraft)
	a.POST("/licenses/:id/submit", s.submit)
	a.POST("/licenses/:id/approve", s.approve)
	a.POST("/licenses/:id/reject", s.reject)
	a.POST("/licenses/:id/revoke", s.revoke)
	a.POST("/licenses/:id/renew", s.renew)
	a.GET("/licenses/:id/download", s.download)
	a.GET("/batches", s.listBatches)
	a.POST("/batches", s.exportBatch)
	a.GET("/batches/:id/request", s.batchRequest)
	a.POST("/batches/:id/cancel", s.cancelBatch)
	a.POST("/batches/import", s.importSigned)
	a.GET("/revocations", s.listRevocations)
	a.GET("/crls", s.listCRLs)
	a.GET("/crls/latest/download", s.downloadCRL)
	a.GET("/keys", s.listKeys)
	a.POST("/keys", s.addKey)
	a.PUT("/keys/:id/status", s.keyStatus)
	a.GET("/users", s.listUsers)
	a.POST("/users", s.createUser)
	a.PUT("/users/:id", s.updateUser)
	a.GET("/audit", s.listAudit)
	a.GET("/audit/verify", s.verifyAudit)

	if s.Web != nil {
		files := http.FileServer(http.FS(s.Web))
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				fail(c, http.StatusNotFound, "NOT_FOUND", "not found")
				return
			}
			if _, err := fs.Stat(s.Web, "index.html"); err != nil {
				c.String(http.StatusServiceUnavailable, "web/portal/dist 未构建：先执行 make web 再编译 "+brand.LicenseServer()+"\n")
				return
			}
			p := strings.TrimPrefix(c.Request.URL.Path, "/")
			if st, err := fs.Stat(s.Web, p); p == "" || err != nil || st.IsDir() {
				c.Request.URL.Path = "/"
			}
			files.ServeHTTP(c.Writer, c.Request)
		})
	}
	return r, nil
}

func (s *Server) secret(ctx context.Context) ([]byte, error) {
	kv := query.Use(s.Store.DB).SystemKV
	if row, err := kv.WithContext(ctx).Where(kv.Key.Eq("jwt_secret")).First(); err == nil {
		return []byte(row.Value), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	v := hex.EncodeToString(b)
	if err := kv.WithContext(ctx).Create(&store.SystemKV{Key: "jwt_secret", Value: v}); err != nil {
		return nil, err
	}
	return []byte(v), nil
}

func securityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; script-src 'self'")
	h.Set("Cache-Control", "no-store")
	c.Next()
}

// csrf：修改类请求必须带自定义头（浏览器跨站表单无法设置），配合 SameSite=Strict Cookie。
func csrf(c *gin.Context) {
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
	case errors.Is(err, service.ErrForbidden):
		fail(c, http.StatusForbidden, "FORBIDDEN", err.Error())
	case errors.Is(err, service.ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "记录不存在")
	case errors.Is(err, service.ErrInvalidState):
		fail(c, http.StatusConflict, "INVALID_STATE", err.Error())
	default:
		fail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	}
}

// ---------- 认证 ----------

type claims struct {
	UID uint `json:"uid"`
	jwt.RegisteredClaims
}

func (s *Server) issue(c *gin.Context, u *store.User) error {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{UID: u.ID, RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(8 * time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}})
	str, err := tok.SignedString(s.jwtKey)
	if err != nil {
		return err
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: str, Path: "/", HttpOnly: true, Secure: s.SecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	return nil
}

// auth 每次请求都从数据库读取用户：角色变更与停用立即生效。
func (s *Server) auth(c *gin.Context) {
	ck, err := c.Cookie(cookieName)
	if err != nil {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	var cl claims
	tok, err := jwt.ParseWithClaims(ck, &cl, func(*jwt.Token) (any, error) { return s.jwtKey, nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !tok.Valid {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "登录已过期")
		return
	}
	u := query.Use(s.Store.DB).User
	row, err := u.WithContext(c.Request.Context()).Where(u.ID.Eq(cl.UID)).First()
	if err != nil || row.Disabled {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "账号不存在或已停用")
		return
	}
	c.Set("user", row)
	c.Next()
}

func actor(c *gin.Context) service.Actor {
	u, _ := c.Get("user")
	if row, ok := u.(*store.User); ok {
		return service.Actor{User: row.Username, Roles: service.Roles(row), IP: c.ClientIP()}
	}
	return service.Actor{IP: c.ClientIP()}
}

type credentials struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (s *Server) setupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"initialized": s.Svc.Initialized(c.Request.Context())})
}

func (s *Server) setup(c *gin.Context) {
	var req credentials
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	u, err := s.Svc.Setup(c.Request.Context(), req.Username, req.Password, c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	if err := s.issue(c, u); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, userView(u))
}

func (s *Server) login(c *gin.Context) {
	ip := c.ClientIP()
	v, _ := s.fails.LoadOrStore(ip, &loginFail{})
	lf := v.(*loginFail)
	lf.mu.Lock()
	locked := time.Now().Before(lf.until)
	lf.mu.Unlock()
	if locked {
		fail(c, http.StatusTooManyRequests, "LOCKED", "登录失败次数过多，请 15 分钟后再试")
		return
	}
	var req credentials
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	u, err := s.Svc.Authenticate(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		lf.mu.Lock()
		if lf.n++; lf.n >= 5 {
			lf.n, lf.until = 0, time.Now().Add(15*time.Minute)
		}
		lf.mu.Unlock()
		fail(c, http.StatusUnauthorized, "BAD_CREDENTIALS", err.Error())
		return
	}
	lf.mu.Lock()
	lf.n = 0
	lf.mu.Unlock()
	if err := s.issue(c, u); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, userView(u))
}

func (s *Server) logout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	c.Status(http.StatusNoContent)
}

func userView(u *store.User) gin.H {
	return gin.H{"id": u.ID, "username": u.Username, "display_name": u.DisplayName, "roles": service.Roles(u)}
}

func (s *Server) me(c *gin.Context) {
	u, _ := c.Get("user")
	c.JSON(http.StatusOK, userView(u.(*store.User)))
}

// ---------- 通用 ----------

func id(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		fail(c, http.StatusBadRequest, "BAD_ID", "invalid id")
		return 0, false
	}
	return uint(n), true
}

func queryUint(c *gin.Context, key string) uint {
	n, _ := strconv.ParseUint(c.Query(key), 10, 32)
	return uint(n)
}

func reply(c *gin.Context, v any, err error) {
	if err != nil {
		failErr(c, err)
		return
	}
	if v == nil {
		c.Status(http.StatusNoContent)
		return
	}
	// 空列表一律返回 []，不返回 null（前端直接对结果调用数组方法）
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		c.JSON(http.StatusOK, []any{})
		return
	}
	c.JSON(http.StatusOK, v)
}

func attachment(c *gin.Context, name, contentType, body string) {
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, contentType, []byte(body))
}

// readBody 读取上传的文件内容（上限 8 MiB）。
func readBody(c *gin.Context) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 8<<20 {
		return nil, errors.New("文件过大")
	}
	return b, nil
}
