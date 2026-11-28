package api

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gen/field"
	"gorm.io/gorm/clause"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// bg 用于不依附单个请求的统计查询。
var bg = context.Background()

// 空档案表示编译期默认安装根目录（layout.DefaultSystemRoot），新建节点组不需要显式保存。
func defaultGroupProfile() string { return "" }

func (s *Server) q() *query.Query { return query.Use(s.Store.DB) }

// ---------- 初始化与登录 ----------

type licenseView struct {
	State          string    `json:"state"`
	Reason         string    `json:"reason"`
	Writable       bool      `json:"writable"`
	LicenseID      string    `json:"license_id,omitempty"`
	Customer       string    `json:"customer,omitempty"`
	Edition        string    `json:"edition,omitempty"`
	Features       []string  `json:"features,omitempty"`
	Nodes          int       `json:"nodes_limit"`
	Sites          int       `json:"sites_limit"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	MaintUntil     time.Time `json:"maintenance_until,omitempty"`
	DaysLeft       int       `json:"days_left"`
	InstallationID string    `json:"installation_id"`
	NodesUsed      int64     `json:"nodes_used"`
	SitesUsed      int64     `json:"sites_used"`
	CRL            any       `json:"crl"` // 当前吊销列表摘要，没有时为 null
}

func (s *Server) licView() licenseView {
	snap := s.Lic.Snapshot()
	v := licenseView{State: string(snap.State), Reason: snap.Reason, Writable: snap.Writable(), InstallationID: s.Lic.InstallationID()}
	if p := snap.Payload; p != nil {
		v.LicenseID, v.Customer, v.Edition = p.LicenseID, p.Customer.Name, string(p.Edition)
		for _, f := range p.Features {
			v.Features = append(v.Features, string(f))
		}
		v.Nodes, v.Sites, v.ExpiresAt, v.MaintUntil = p.Limits.Nodes, p.Limits.Sites, p.ExpiresAt, p.MaintenanceUntil
		v.DaysLeft = int(time.Until(p.ExpiresAt).Hours() / 24)
	}
	if info := s.Lic.CRLInfo(); info != nil {
		v.CRL = info
	}
	q := s.q()
	v.NodesUsed, _ = q.Node.WithContext(bg).Where(q.Node.Status.Neq("revoked")).Count()
	v.SitesUsed, _ = q.Site.WithContext(bg).Count()
	return v
}

func (s *Server) userCount() int64 {
	n, _ := s.q().User.WithContext(bg).Count()
	return n
}

func (s *Server) setupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"initialized": s.userCount() > 0, "license": s.licView(), "oidc": s.oidcEnabled()})
}

type credentials struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// setupDoneKey 是首次初始化的唯一标记（system_kvs 的主键），见 setup。
const setupDoneKey = "setup.done"

func (s *Server) setup(c *gin.Context) {
	if s.userCount() > 0 {
		fail(c, http.StatusConflict, "ALREADY_INITIALIZED", "already initialized")
		return
	}
	var req credentials
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := render.ValidateName(strings.ToLower(req.Username)); err != nil || req.Username != strings.ToLower(req.Username) {
		fail(c, http.StatusBadRequest, "BAD_USERNAME", "用户名只能包含小写字母、数字和连字符，2–63 位")
		return
	}
	if err := s.policy().CheckPassword(req.Username, req.Password); err != nil {
		fail(c, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
		return
	}
	h, err := crypto.HashPassword(req.Password)
	if err != nil {
		failErr(c, err)
		return
	}
	now := time.Now()
	u := &store.User{Username: req.Username, PasswordHash: h, Role: store.RoleAdmin, Source: "local", PasswordChangedAt: &now}
	// 初始化必须只成功一次：用户数检查与创建之间有间隙，并发请求会各自通过检查。
	// 在同一个事务里先抢占唯一键 setup.done（跨进程、SQLite 与 PostgreSQL 都只有一个请求能插入成功），
	// 抢到的再创建管理员与默认节点组。事务回滚时标记一并撤销，失败后可以重新初始化。
	errAlready := errors.New("already initialized")
	err = s.q().Transaction(func(tx *query.Query) error {
		ctx := c.Request.Context()
		// 用本次请求独有的值占位：DoNothing 不报冲突，读回来仍是自己的值才说明抢到了
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		mine := now.UTC().Format(time.RFC3339) + "-" + hex.EncodeToString(nonce)
		kv := tx.SystemKV.WithContext(ctx)
		if err := kv.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.SystemKV{Key: setupDoneKey, Value: mine}); err != nil {
			return err
		}
		got, err := kv.Where(tx.SystemKV.Key.Eq(setupDoneKey)).First()
		if err != nil {
			return err
		}
		if got.Value != mine {
			return errAlready
		}
		if n, err := tx.User.WithContext(ctx).Count(); err != nil {
			return err
		} else if n > 0 {
			return errAlready
		}
		if err := tx.User.WithContext(ctx).Create(u); err != nil {
			return err
		}
		// 初始化默认节点组
		if n, err := tx.NodeGroup.WithContext(ctx).Count(); err == nil && n == 0 {
			return tx.NodeGroup.WithContext(ctx).Create(&store.NodeGroup{Name: "default", Description: "默认节点组",
				AllowedCIDRs: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16", InstanceProfile: defaultGroupProfile()})
		}
		return nil
	})
	if errors.Is(err, errAlready) {
		fail(c, http.StatusConflict, "ALREADY_INITIALIZED", "already initialized")
		return
	}
	if err != nil {
		failErr(c, err)
		return
	}
	c.Set("user", u.Username)
	s.audit(c, "system.setup", "user", u.Username, nil)
	_ = s.issue(c, u)
	c.JSON(http.StatusOK, gin.H{"username": u.Username, "role": u.Role})
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
	uq, ctx := s.q().User, c.Request.Context()
	pol := s.policy()
	u, err := uq.WithContext(ctx).Where(uq.Username.Eq(req.Username)).First()
	if err == nil && u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
		c.Set("user", req.Username)
		_ = s.Audit.Write(auditEntry(c, "auth.login", "user", req.Username, "locked"))
		fail(c, http.StatusTooManyRequests, "ACCOUNT_LOCKED", fmt.Sprintf("账号已锁定，请于 %s 后再试或联系管理员解锁", u.LockedUntil.Format("15:04")))
		return
	}
	var ok bool
	switch {
	case err == nil && isLocal(u):
		ok = crypto.CheckPassword(u.PasswordHash, req.Password)
	case s.ldapEnabled() && (err != nil || u.Source == "ldap"):
		// 本地没有此用户或用户来自 LDAP：交给 LDAP 认证（首次登录自动创建，角色按映射）
		u, ok = s.ldapLogin(c, req.Username, req.Password)
		err = nil
	}
	if err == nil && ok && u.Disabled {
		c.Set("user", u.Username)
		_ = s.Audit.Write(auditEntry(c, "auth.login", "user", u.Username, "disabled"))
		fail(c, http.StatusForbidden, "ACCOUNT_DISABLED", "账号已停用")
		return
	}
	if !ok {
		lf.mu.Lock()
		if lf.n++; lf.n >= 5*2 { // 单个来源 IP 的失败上限（防撞库），账号级锁定见下
			lf.n, lf.until = 0, time.Now().Add(15*time.Minute)
		}
		lf.mu.Unlock()
		if err == nil && u != nil && u.ID != 0 {
			n := u.FailedLogins + 1
			set := []field.AssignExpr{uq.FailedLogins.Value(n)}
			if n >= pol.LockThreshold {
				set = []field.AssignExpr{uq.FailedLogins.Value(0), uq.LockedUntil.Value(time.Now().Add(time.Duration(pol.LockMinutes) * time.Minute))}
			}
			_, _ = uq.WithContext(ctx).Where(uq.ID.Eq(u.ID)).UpdateSimple(set...)
		}
		c.Set("user", req.Username)
		_ = s.Audit.Write(auditEntry(c, "auth.login", "user", req.Username, "failure"))
		fail(c, http.StatusUnauthorized, "BAD_CREDENTIALS", "用户名或密码错误")
		return
	}
	lf.mu.Lock()
	lf.n = 0
	lf.mu.Unlock()
	_, _ = uq.WithContext(ctx).Where(uq.ID.Eq(u.ID)).UpdateSimple(uq.LastLoginAt.Value(time.Now()), uq.FailedLogins.Value(0), uq.LockedUntil.Null())
	c.Set("user", u.Username)
	s.audit(c, "auth.login", "user", u.Username, map[string]string{"source": u.Source})
	if err := s.issue(c, u); err != nil {
		failErr(c, err)
		return
	}
	mustChange := u.MustChangePassword || (isLocal(u) && pol.Expired(u.PasswordChangedAt, u.CreatedAt, time.Now()))
	c.JSON(http.StatusOK, gin.H{"username": u.Username, "role": u.Role, "must_change_password": mustChange})
}

// logout 清除 cookie，并让该用户已签发的全部会话失效（令牌无状态，只能靠递增账号的会话版本）。
// 没有有效令牌的请求只清 cookie，不影响任何账号。
func (s *Server) logout(c *gin.Context) {
	if ck, err := c.Cookie(cookieName); err == nil {
		var cl claims
		if tok, err := jwt.ParseWithClaims(ck, &cl, func(t *jwt.Token) (any, error) { return s.jwtKey, nil }, jwt.WithValidMethods([]string{"HS256"})); err == nil && tok.Valid {
			uq := s.q().User
			if _, err := uq.WithContext(c.Request.Context()).Where(uq.ID.Eq(cl.UID), uq.TokenVersion.Eq(cl.TV)).UpdateSimple(uq.TokenVersion.Add(1)); err != nil {
				s.Log.Error("failed to revoke sessions on logout", "uid", cl.UID, "err", err)
			}
		}
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode})
	c.Status(http.StatusNoContent)
}

func (s *Server) me(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"username": c.GetString("user"), "role": c.GetString("role"), "must_change_password": c.GetBool("pc")})
}

// ---------- 授权 ----------

func (s *Server) licenseGet(c *gin.Context) { c.JSON(http.StatusOK, s.licView()) }

func (s *Server) licenseRequest(c *gin.Context) {
	c.Header("Content-Disposition", `attachment; filename="activation-request.json"`)
	c.JSON(http.StatusOK, s.Lic.ActivationRequest())
}

func (s *Server) licenseImport(c *gin.Context) {
	var req struct {
		Raw string `json:"raw" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	snap, err := s.Lic.Import(req.Raw, c.GetString("user"))
	if err != nil {
		fail(c, http.StatusBadRequest, "LICENSE_REJECTED", err.Error())
		return
	}
	id := ""
	if snap.Payload != nil {
		id = snap.Payload.LicenseID
	}
	detail := map[string]any{"state": string(snap.State)}
	if info := s.Lic.CRLInfo(); info != nil {
		detail["crl_serial"] = info.Serial
	}
	s.audit(c, "license.import", "license", id, detail)
	go s.pushLicense()
	c.JSON(http.StatusOK, s.licView())
}

// licenseCRL 单独导入吊销列表（授权过期 / 无效时仍可用）。
func (s *Server) licenseCRL(c *gin.Context) {
	var req struct {
		Raw string `json:"raw" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	crl, err := s.Lic.ImportCRL(req.Raw)
	if err != nil {
		fail(c, http.StatusBadRequest, "CRL_REJECTED", err.Error())
		return
	}
	s.audit(c, "license.crl_import", "crl", fmt.Sprint(crl.Serial), map[string]any{"revoked": len(crl.RevokedLicenses), "revoked_kids": crl.RevokedKIDs})
	go s.pushLicense()
	c.JSON(http.StatusOK, s.licView())
}

// ---------- 节点组 ----------

func parseID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		fail(c, http.StatusBadRequest, "BAD_ID", "invalid id")
		return 0, false
	}
	return uint(id), true
}

func queryGroup(c *gin.Context) uint {
	id, _ := strconv.ParseUint(c.Query("group_id"), 10, 32)
	return uint(id)
}

type groupReq struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	AllowedCIDRs string `json:"allowed_cidrs"`
	RealIPFrom   string `json:"real_ip_from"`
	VIPs         string `json:"vips"`
}

func validCIDRs(csv string) error {
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if _, err := netip.ParsePrefix(p); err != nil {
			return fmt.Errorf("invalid CIDR %q", p)
		}
	}
	return nil
}

func (s *Server) listGroups(c *gin.Context) {
	q, ctx := s.q(), c.Request.Context()
	gs, err := q.NodeGroup.WithContext(ctx).Order(q.NodeGroup.ID).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	type row struct {
		store.NodeGroup
		Nodes          int64  `json:"nodes"`
		Online         int64  `json:"online"`
		Sites          int64  `json:"sites"`
		ManagementMode string `json:"management_mode"`
		HandoffPending bool   `json:"handoff_pending"`
	}
	out := make([]row, 0, len(gs))
	for _, g := range gs {
		r := row{NodeGroup: *g}
		mode, err := s.Deploy.ManagementModeView(g.ID)
		if err != nil {
			failErr(c, err)
			return
		}
		r.ManagementMode, r.HandoffPending = mode.Mode, mode.HandoffPending
		nd := q.Node
		r.Nodes, _ = nd.WithContext(ctx).Where(nd.GroupID.Eq(g.ID), nd.Status.Neq("revoked")).Count()
		r.Online, _ = nd.WithContext(ctx).Where(nd.GroupID.Eq(g.ID), nd.Status.Eq("online")).Count()
		r.Sites, _ = q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(g.ID)).Count()
		out = append(out, r)
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createGroup(c *gin.Context) {
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := render.ValidateName(req.Name); err != nil {
		failErr(c, err)
		return
	}
	if err := validCIDRs(req.AllowedCIDRs + "," + req.RealIPFrom); err != nil {
		failErr(c, err)
		return
	}
	vips, err := normalizeVIPs(req.VIPs)
	if err != nil {
		failErr(c, err)
		return
	}
	if s.groupNameTaken(c, req.Name, 0) {
		return
	}
	g := &store.NodeGroup{Name: req.Name, Description: req.Description, AllowedCIDRs: req.AllowedCIDRs, RealIPFrom: req.RealIPFrom, VIPs: vips, InstanceProfile: defaultGroupProfile()}
	if err := s.q().NodeGroup.WithContext(c.Request.Context()).Create(g); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "group.create", "node_group", fmt.Sprint(g.ID), req)
	c.JSON(http.StatusOK, g)
}

func (s *Server) updateGroup(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := validCIDRs(req.AllowedCIDRs + "," + req.RealIPFrom); err != nil {
		failErr(c, err)
		return
	}
	vips, err := normalizeVIPs(req.VIPs)
	if err != nil {
		failErr(c, err)
		return
	}
	g, err := store.First[store.NodeGroup](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	// 改名：名称只用于显示与审计，配置与节点按编号关联，改名不影响已注册的节点与已发布的版本
	name := g.Name
	if req.Name != "" && req.Name != g.Name {
		if err := render.ValidateName(req.Name); err != nil {
			failErr(c, err)
			return
		}
		if s.groupNameTaken(c, req.Name, g.ID) {
			return
		}
		name = req.Name
	}
	ng := s.q().NodeGroup
	if _, err := ng.WithContext(c.Request.Context()).Where(ng.ID.Eq(g.ID)).UpdateSimple(ng.Name.Value(name), ng.Description.Value(req.Description),
		ng.AllowedCIDRs.Value(req.AllowedCIDRs), ng.RealIPFrom.Value(req.RealIPFrom), ng.VIPs.Value(vips)); err != nil {
		failErr(c, err)
		return
	}
	g.Name, g.Description, g.AllowedCIDRs, g.RealIPFrom, g.VIPs = name, req.Description, req.AllowedCIDRs, req.RealIPFrom, vips
	s.audit(c, "group.update", "node_group", fmt.Sprint(id), req)
	c.JSON(http.StatusOK, g)
}

// groupNameTaken 检查节点组名称是否已被其他组使用（已写入 409 响应时返回 true）。
func (s *Server) groupNameTaken(c *gin.Context, name string, self uint) bool {
	ng := s.q().NodeGroup
	n, err := ng.WithContext(c.Request.Context()).Where(ng.Name.Eq(name), ng.ID.Neq(self)).Count()
	if err != nil {
		failErr(c, err)
		return true
	}
	if n > 0 {
		fail(c, http.StatusConflict, "NAME_TAKEN", "节点组名称已存在")
		return true
	}
	return false
}

// groupUsage 返回删除前的占用情况：哪些阻止删除（未吊销的节点、进行中的操作、最后一个组），哪些会随组删除。
func (s *Server) groupUsage(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	u, err := s.Deploy.GroupUsage(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}

// deleteGroup 删除节点组（仅管理员）。请求体须带 {"confirm": "<组名>"}，防止误删；审计由 deploy.DeleteGroup 写入。
func (s *Server) deleteGroup(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
	}
	_ = c.ShouldBindJSON(&req)
	g, err := store.First[store.NodeGroup](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if req.Confirm != g.Name {
		fail(c, http.StatusBadRequest, "CONFIRM_MISMATCH", "请输入节点组名称以确认删除")
		return
	}
	u, err := s.Deploy.DeleteGroup(id, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}

func (s *Server) enrollToken(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if _, err := store.First[store.NodeGroup](s.Store, id); err != nil {
		failErr(c, err)
		return
	}
	snap := s.Lic.Snapshot()
	if snap.Payload != nil && snap.Payload.Limits.Nodes > 0 {
		nd := s.q().Node
		n, err := nd.WithContext(c.Request.Context()).Where(nd.Status.Neq("revoked")).Count()
		if err != nil {
			failErr(c, err)
			return
		}
		if n >= int64(snap.Payload.Limits.Nodes) {
			fail(c, http.StatusPaymentRequired, "LICENSE_NODE_LIMIT", fmt.Sprintf("已达到授权节点数上限（%d）", snap.Payload.Limits.Nodes))
			return
		}
	}
	tok, exp, err := s.GW.NewEnrollToken(id, c.GetString("user"))
	if err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "node.enroll_token", "node_group", fmt.Sprint(id), map[string]any{"expires_at": exp})
	prof, err := s.Deploy.Profile(id)
	if err != nil {
		failErr(c, err)
		return
	}
	management, err := s.Deploy.ManagementModeView(id)
	if err != nil {
		failErr(c, err)
		return
	}
	mode := prof.Mode
	if mode == "" {
		mode = "system"
	}
	c.JSON(http.StatusOK, gin.H{"token": tok, "expires_at": exp, "server": s.GRPCAddr, "ca_fingerprint": s.CAFingerprint,
		"command":          enrollCommand(prof, s.GRPCAddr, tok, s.CAFingerprint),
		"existing_command": enrollExistingCommand(prof, s.GRPCAddr, tok, s.CAFingerprint), "mode": mode, "instance": prof.Instance, "root": prof.Root,
		"http_port": prof.HTTP(), "https_port": prof.HTTPS(), "status_port": prof.Status(),
		"management_mode": management.Mode, "management_label": management.Label, "management_description": management.Description})
}

// enrollExistingCommand registers an already-created instance without trying to
// create it again. The local configuration flag belongs to the agent command,
// before any shell chaining.
func enrollExistingCommand(p render.Profile, server, token, caFP string) string {
	reg := fmt.Sprintf("-server %s -token %s -ca-fingerprint %s -replace-local-config", server, token, caFP)
	if p.Mode == "user" {
		instance := ""
		if p.Instance != "" {
			instance = " -instance " + p.Instance
		}
		return fmt.Sprintf("%s/bin/%s enroll -root %s%s %s && %s/bin/%s-instance --root %s ensure",
			p.Root, brand.Agent(), p.Root, instance, reg, p.Root, brand.ID(), p.Root)
	}
	if p.Instance == "" {
		return fmt.Sprintf("sudo %s enroll %s && sudo systemctl enable %s && sudo systemctl start %s",
			brand.Agent(), reg, brand.Agent(), brand.Agent())
	}
	l := layout.SystemAt(p.Root, p.Instance)
	unit := strings.TrimSuffix(l.AgentUnit(), ".service")
	return fmt.Sprintf("sudo runuser -u %s -- %s enroll -instance %s %s && sudo systemctl enable %s && sudo systemctl start %s",
		l.AgentUser(), l.AgentBin(), p.Instance, reg, unit, unit)
}

// enrollCommand 返回在节点上执行的注册命令：默认实例注册已安装的 agent；
// 具名实例用 <品牌>-instance add 按节点组档案创建实例并注册（设计 0001 第 5.7 节）。
// user 模式（设计 0001 第 6 节）：以安装用户执行，不用 sudo，命令都带安装根目录。
func enrollCommand(p render.Profile, server, token, caFP string) string {
	if p.Mode == "user" {
		r := p.Root
		reg := fmt.Sprintf("-server %s -token %s -ca-fingerprint %s", server, token, caFP)
		if p.Instance == "" {
			return fmt.Sprintf("%s/bin/%s enroll -root %s %s && %s/bin/%s-instance --root %s ensure", r, brand.Agent(), r, reg, r, brand.ID(), r)
		}
		cmd := fmt.Sprintf("%s/bin/%s-instance --root %s add %s --http %d --https %d --status %d", r, brand.ID(), r, p.Instance, p.HTTP(), p.HTTPS(), p.Status())
		if p.ResourceMode == "independent" {
			cmd += " --resource-mode independent"
		}
		if p.NoFile != 0 {
			cmd += fmt.Sprintf(" --nofile %d", p.NoFile)
		}
		return cmd + fmt.Sprintf(" --console %s --token %s --ca-fingerprint %s", server, token, caFP)
	}
	if p.Instance == "" {
		// 不用 systemctl enable --now：systemd 220 以下（CentOS 7.0/7.1）不支持
		return fmt.Sprintf("sudo %s enroll -server %s -token %s -ca-fingerprint %s && sudo systemctl enable %s && sudo systemctl start %s",
			brand.Agent(), server, token, caFP, brand.Agent(), brand.Agent())
	}
	cmd := fmt.Sprintf("sudo %s-instance add %s --http %d --https %d --status %d", brand.ID(), p.Instance, p.HTTP(), p.HTTPS(), p.Status())
	if p.ResourceMode == "independent" {
		cmd += " --resource-mode independent"
	}
	if p.NoFile != 0 {
		cmd += fmt.Sprintf(" --nofile %d", p.NoFile)
	}
	return cmd + fmt.Sprintf(" --console %s --token %s --ca-fingerprint %s", server, token, caFP)
}

// ---------- 节点 ----------

func (s *Server) listNodes(c *gin.Context) {
	ctx := c.Request.Context()
	q := s.q()
	nd := q.Node
	sel := nd.WithContext(ctx).Order(nd.ID)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(nd.GroupID.Eq(g))
	}
	ns, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	gs, _ := q.NodeGroup.WithContext(ctx).Find()
	groupVIPs := map[uint]string{}
	for _, g := range gs {
		if g.VIPs != "" {
			groupVIPs[g.ID] = g.VIPs
		}
	}
	type nodeView struct {
		*store.Node
		HeldVIPs []string `json:"held_vips,omitempty"`
		VIPRole  string   `json:"vip_role,omitempty"` // master | backup | ""
	}
	out := make([]nodeView, 0, len(ns))
	for _, n := range ns {
		v := nodeView{Node: n}
		if gv := groupVIPs[n.GroupID]; gv != "" && n.Status == "online" {
			held := heldVIPs(n.LocalIPs, gv)
			if len(held) > 0 {
				v.HeldVIPs = held
				v.VIPRole = "master"
			} else {
				v.VIPRole = "backup"
			}
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) restoreNode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dep, err := s.Deploy.Restore(id, c.GetString("user"), c.ClientIP())
	switch {
	case errors.Is(err, deploy.ErrNotDrifted):
		fail(c, http.StatusConflict, "NOT_DRIFTED", "该节点没有手工改动，无需恢复")
	case errors.Is(err, deploy.ErrBusy):
		fail(c, http.StatusConflict, "BUSY", "节点组正在发布，请稍后再试")
	case err != nil:
		failErr(c, err)
	default:
		c.JSON(http.StatusOK, dep)
	}
}

func (s *Server) revokeNode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	nd := s.q().Node
	if _, err := nd.WithContext(c.Request.Context()).Where(nd.ID.Eq(id)).Update(nd.Status, "revoked"); err != nil {
		failErr(c, err)
		return
	}
	s.GW.Disconnect(id)
	s.audit(c, "node.revoke", "node", fmt.Sprint(id), nil)
	c.Status(http.StatusNoContent)
}

// ---------- 站点 ----------

type siteView struct {
	store.Site
	Spec render.Site `json:"spec"`
}

// stripPEM：站点只保存证书库编号，证书与私钥原文不落在站点配置里（规格书 9.3）。
func stripPEM(site *render.Site) {
	if site.HTTPS != nil {
		site.HTTPS = &render.TLS{CertID: site.HTTPS.CertID, GMCertID: site.HTTPS.GMCertID}
	}
}

// prepareRules 检查 Lua 规则授权并加密 JWT 密钥（失败时已写入响应）。
func (s *Server) prepareRules(c *gin.Context, spec *render.Site, old *render.Site) error {
	if (render.Input{Sites: []render.Site{*spec}}).HasLua() && !s.Lic.Snapshot().Feature(license.FeatureLua) {
		fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", "当前授权不包含 Lua 规则（lua），JWT 规则需要专业版或企业版")
		return errors.New("denied")
	}
	if err := s.Deploy.SealRules(spec, old); err != nil {
		failErr(c, err)
		return err
	}
	return nil
}

func toSiteView(st store.Site) siteView {
	v := siteView{Site: st}
	_ = json.Unmarshal([]byte(st.Spec), &v.Spec)
	v.Spec.Name = st.Name
	if v.Spec.HTTPS != nil { // 私钥不回显（规格书 9.3）
		v.Spec.HTTPS.KeyPEM = ""
	}
	deploy.RedactRules(&v.Spec) // JWT 密钥（明文与密文）不回显
	return v
}

func (s *Server) listSites(c *gin.Context) {
	sq := s.q().Site
	sel := sq.WithContext(c.Request.Context()).Order(sq.Name)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(sq.GroupID.Eq(g))
	}
	ss, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	out := make([]siteView, 0, len(ss))
	for _, st := range ss {
		out = append(out, toSiteView(*st))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getSite(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	st, err := store.First[store.Site](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toSiteView(*st))
}

type siteReq struct {
	GroupID uint        `json:"group_id"`
	Enabled *bool       `json:"enabled"`
	Version int         `json:"version"`
	Spec    render.Site `json:"spec"`
}

// validateWith 把待保存的站点代入整个节点组渲染一次，借渲染器完成全部校验。
func (s *Server) validateWith(groupID uint, site *render.Site, upstream *render.UpstreamGroup, dropSite, dropUp string) error {
	in, err := s.Deploy.Input(groupID, "r000000")
	if err != nil {
		return err
	}
	if site != nil || dropSite != "" {
		var keep []render.Site
		for _, x := range in.Sites {
			if (site == nil || x.Name != site.Name) && x.Name != dropSite {
				keep = append(keep, x)
			}
		}
		if site != nil {
			sc := *site
			if sc.HTTPS != nil {
				t := *sc.HTTPS
				sc.HTTPS = &t
			}
			if err := s.Certs.ResolveSite(&sc); err != nil {
				return err
			}
			if sc.HTTPS != nil && sc.HTTPS.GM != nil {
				if err := s.Deploy.CheckGM(groupID); err != nil {
					return err
				}
			}
			if err := s.Deploy.OpenRules(&sc); err != nil {
				return err
			}
			keep = append(keep, sc)
			if (render.Input{Sites: []render.Site{sc}}).HasLua() {
				if err := s.Deploy.CheckLua(groupID); err != nil {
					return err
				}
			}
		}
		in.Sites = keep
	}
	if upstream != nil || dropUp != "" {
		var keep []render.UpstreamGroup
		for _, x := range in.Upstreams {
			if (upstream == nil || x.Name != upstream.Name) && x.Name != dropUp {
				keep = append(keep, x)
			}
		}
		if upstream != nil {
			keep = append(keep, *upstream)
		}
		in.Upstreams = keep
	}
	_, err = render.Render(in)
	return err
}

func (s *Server) createSite(c *gin.Context) {
	var req siteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if _, err := store.First[store.NodeGroup](s.Store, req.GroupID); err != nil {
		failErr(c, err)
		return
	}
	snap := s.Lic.Snapshot()
	if snap.Payload != nil && snap.Payload.Limits.Sites > 0 {
		n, err := s.q().Site.WithContext(c.Request.Context()).Count()
		if err != nil {
			failErr(c, err)
			return
		}
		if n >= int64(snap.Payload.Limits.Sites) {
			fail(c, http.StatusPaymentRequired, "LICENSE_SITE_LIMIT", fmt.Sprintf("已达到授权站点数上限（%d）", snap.Payload.Limits.Sites))
			return
		}
	}
	stripPEM(&req.Spec)
	if err := s.prepareRules(c, &req.Spec, nil); err != nil {
		return
	}
	if err := s.validateWith(req.GroupID, &req.Spec, nil, "", ""); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_SITE", err.Error())
		return
	}
	spec, _ := json.Marshal(req.Spec)
	st := &store.Site{GroupID: req.GroupID, Name: req.Spec.Name, Spec: string(spec), Enabled: req.Enabled == nil || *req.Enabled, Version: 1, UpdatedBy: c.GetString("user")}
	if err := s.q().Site.WithContext(c.Request.Context()).Create(st); err != nil {
		fail(c, http.StatusConflict, "DUPLICATE", "同名站点已存在")
		return
	}
	s.audit(c, "site.create", "site", st.Name, map[string]any{"group_id": req.GroupID, "domains": req.Spec.Domains})
	c.JSON(http.StatusOK, toSiteView(*st))
}

func (s *Server) updateSite(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req siteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	st, err := store.First[store.Site](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if req.Version != st.Version {
		fail(c, http.StatusConflict, "VERSION_CONFLICT", "站点已被他人修改，请刷新后重试")
		return
	}
	req.Spec.Name = st.Name
	stripPEM(&req.Spec)
	var old render.Site
	_ = json.Unmarshal([]byte(st.Spec), &old)
	if err := s.prepareRules(c, &req.Spec, &old); err != nil {
		return
	}
	enabled := st.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if enabled {
		if err := s.validateWith(st.GroupID, &req.Spec, nil, "", ""); err != nil {
			fail(c, http.StatusBadRequest, "INVALID_SITE", err.Error())
			return
		}
	}
	spec, _ := json.Marshal(req.Spec)
	// 乐观锁：只有版本号未变时才更新
	sq := s.q().Site
	res, err := sq.WithContext(c.Request.Context()).Where(sq.ID.Eq(id), sq.Version.Eq(st.Version)).UpdateSimple(sq.Spec.Value(string(spec)),
		sq.Enabled.Value(enabled), sq.Version.Value(st.Version+1), sq.UpdatedBy.Value(c.GetString("user")))
	if err != nil {
		failErr(c, err)
		return
	}
	if res.RowsAffected != 1 {
		fail(c, http.StatusConflict, "VERSION_CONFLICT", "站点已被他人修改，请刷新后重试")
		return
	}
	s.audit(c, "site.update", "site", st.Name, map[string]any{"domains": req.Spec.Domains, "enabled": enabled})
	st, _ = store.First[store.Site](s.Store, id)
	c.JSON(http.StatusOK, toSiteView(*st))
}

func (s *Server) deleteSite(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	st, err := store.First[store.Site](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if _, err := s.q().Site.WithContext(c.Request.Context()).Delete(st); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "site.delete", "site", st.Name, nil)
	c.Status(http.StatusNoContent)
}

// ---------- 证书 ----------

type certReq struct {
	Name    string `json:"name"`
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

func (s *Server) listCerts(c *gin.Context) {
	list, err := s.Certs.List()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

type gmCertReq struct {
	Name string `json:"name"`
	certs.GMInput
}

// createGMCert 上传国密 TLCP 双证书（需要 gm_tls 授权，由路由中间件检查）。
func (s *Server) createGMCert(c *gin.Context) {
	var req gmCertReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	cert, err := s.Certs.CreateGM(strings.TrimSpace(req.Name), req.GMInput, c.GetString("user"))
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_CERTIFICATE", err.Error())
		return
	}
	c.JSON(http.StatusOK, cert)
}

// renewGMCert 续期国密双证书：与标准证书续期一样在任何授权状态下可用（规格书 8.6），续期后自动下发。
func (s *Server) renewGMCert(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req certs.GMInput
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	cert, err := s.Certs.RenewGM(id, req, c.GetString("user"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failErr(c, err)
			return
		}
		fail(c, http.StatusBadRequest, "INVALID_CERTIFICATE", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"certificate": cert, "deployments": s.refreshCert(c, id)})
}

func (s *Server) createCert(c *gin.Context) {
	var req certReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	cert, err := s.Certs.Create(strings.TrimSpace(req.Name), req.CertPEM, req.KeyPEM, c.GetString("user"))
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_CERTIFICATE", err.Error())
		return
	}
	c.JSON(http.StatusOK, cert)
}

type certDeployResult struct {
	GroupID      uint   `json:"group_id"`
	DeploymentID uint   `json:"deployment_id,omitempty"`
	Status       string `json:"status"` // started | unchanged | busy | failed
	Message      string `json:"message,omitempty"`
}

// refreshCert 向所有引用该证书的节点组下发新证书。
func (s *Server) refreshCert(c *gin.Context, id uint) []certDeployResult {
	out := []certDeployResult{}
	for _, g := range s.Certs.Groups(id) {
		r := certDeployResult{GroupID: g}
		dep, err := s.Deploy.CertRefresh(g, id, c.GetString("user"), c.ClientIP())
		switch {
		case errors.Is(err, deploy.ErrBusy):
			r.Status, r.Message = "busy", "该节点组正在发布，稍后在证书页点“重新下发”"
		case err != nil:
			r.Status, r.Message = "failed", err.Error()
		case dep == nil:
			r.Status = "unchanged"
		default:
			r.Status, r.DeploymentID = "started", dep.ID
		}
		out = append(out, r)
	}
	return out
}

func (s *Server) renewCert(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req certReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	cert, err := s.Certs.Renew(id, req.CertPEM, req.KeyPEM, c.GetString("user"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failErr(c, err)
			return
		}
		fail(c, http.StatusBadRequest, "INVALID_CERTIFICATE", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"certificate": cert, "deployments": s.refreshCert(c, id)})
}

func (s *Server) deployCert(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if _, err := store.First[store.Certificate](s.Store, id); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deployments": s.refreshCert(c, id)})
}

func (s *Server) deleteCert(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := s.Certs.Delete(id, c.GetString("user")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failErr(c, err)
			return
		}
		fail(c, http.StatusConflict, "CERTIFICATE_IN_USE", err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------- ACME ----------

func (s *Server) acmeGet(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"settings": s.Acme.GetSettings(), "orders": s.Acme.Orders(20)})
}

func (s *Server) acmePut(c *gin.Context) {
	var req struct {
		Directory string `json:"directory"`
		Email     string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := s.Acme.SaveSettings(req.Directory, req.Email, c.GetString("user")); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_ACME_SETTINGS", err.Error())
		return
	}
	c.JSON(http.StatusOK, s.Acme.GetSettings())
}

func (s *Server) acmeIssue(c *gin.Context) {
	var req struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	o, err := s.Acme.Issue(strings.TrimSpace(req.Name), req.Domains, c.GetString("user"))
	if err != nil {
		fail(c, http.StatusBadRequest, "ACME_REJECTED", err.Error())
		return
	}
	c.JSON(http.StatusAccepted, o)
}

func (s *Server) acmeRenew(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	o, err := s.Acme.Renew(id, c.GetString("user"))
	if errors.Is(err, store.ErrNotFound) {
		failErr(c, err)
		return
	}
	if err != nil {
		fail(c, http.StatusBadRequest, "ACME_REJECTED", err.Error())
		return
	}
	c.JSON(http.StatusAccepted, o)
}

// ---------- 上游 ----------

type upstreamReq struct {
	GroupID uint                 `json:"group_id"`
	Spec    render.UpstreamGroup `json:"spec"`
}

type upstreamView struct {
	store.Upstream
	Spec render.UpstreamGroup `json:"spec"`
}

func (s *Server) listUpstreams(c *gin.Context) {
	uq := s.q().Upstream
	sel := uq.WithContext(c.Request.Context()).Order(uq.Name)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(uq.GroupID.Eq(g))
	}
	us, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	out := make([]upstreamView, 0, len(us))
	for _, u := range us {
		v := upstreamView{Upstream: *u}
		_ = json.Unmarshal([]byte(u.Spec), &v.Spec)
		v.Spec.Name = u.Name
		out = append(out, v)
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createUpstream(c *gin.Context) {
	var req upstreamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := s.validateWith(req.GroupID, nil, &req.Spec, "", ""); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_UPSTREAM", err.Error())
		return
	}
	spec, _ := json.Marshal(req.Spec)
	u := &store.Upstream{GroupID: req.GroupID, Name: req.Spec.Name, Spec: string(spec), UpdatedBy: c.GetString("user")}
	if err := s.q().Upstream.WithContext(c.Request.Context()).Create(u); err != nil {
		fail(c, http.StatusConflict, "DUPLICATE", "同名上游已存在")
		return
	}
	s.audit(c, "upstream.create", "upstream", u.Name, req.Spec)
	c.JSON(http.StatusOK, upstreamView{Upstream: *u, Spec: req.Spec})
}

func (s *Server) updateUpstream(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req upstreamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	u, err := store.First[store.Upstream](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	req.Spec.Name = u.Name
	if err := s.validateWith(u.GroupID, nil, &req.Spec, "", ""); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_UPSTREAM", err.Error())
		return
	}
	spec, _ := json.Marshal(req.Spec)
	uq := s.q().Upstream
	if _, err := uq.WithContext(c.Request.Context()).Where(uq.ID.Eq(u.ID)).
		UpdateSimple(uq.Spec.Value(string(spec)), uq.UpdatedBy.Value(c.GetString("user"))); err != nil {
		failErr(c, err)
		return
	}
	u.Spec, u.UpdatedBy = string(spec), c.GetString("user")
	s.audit(c, "upstream.update", "upstream", u.Name, req.Spec)
	c.JSON(http.StatusOK, upstreamView{Upstream: *u, Spec: req.Spec})
}

func (s *Server) deleteUpstream(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	u, err := store.First[store.Upstream](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if err := s.validateWith(u.GroupID, nil, nil, "", u.Name); err != nil {
		fail(c, http.StatusConflict, "IN_USE", "上游仍被站点引用："+err.Error())
		return
	}
	if _, err := s.q().Upstream.WithContext(c.Request.Context()).Delete(u); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "upstream.delete", "upstream", u.Name, nil)
	c.Status(http.StatusNoContent)
}

// ---------- 发布 ----------

func (s *Server) preview(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	diffs, digest, err := s.Deploy.Preview(id)
	if err != nil {
		fail(c, http.StatusBadRequest, "RENDER_FAILED", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": diffs, "digest": digest, "changed": len(diffs) > 0,
		"comparison_basis": "console_revision"})
}

func (s *Server) publish(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	dep, err := s.Deploy.Publish(id, req.Note, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dep)
}

func (s *Server) rollback(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		RevisionID string `json:"revision_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	dep, err := s.Deploy.Rollback(id, req.RevisionID, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dep)
}

func (s *Server) listRevisions(c *gin.Context) {
	rv := s.q().Revision
	// 列表不读取版本包（Bundle 可能很大）
	sel := rv.WithContext(c.Request.Context()).Select(rv.Seq, rv.ID, rv.GroupID, rv.ParentID, rv.Digest, rv.Note, rv.CreatedBy, rv.CreatedAt).
		Order(rv.Seq.Desc()).Limit(50)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(rv.GroupID.Eq(g))
	}
	rs, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, rs)
}

func (s *Server) listDeployments(c *gin.Context) {
	dp := s.q().Deployment
	sel := dp.WithContext(c.Request.Context()).Preload(dp.Nodes).Order(dp.ID.Desc()).Limit(30)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(dp.GroupID.Eq(g))
	}
	ds, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, ds)
}

func (s *Server) getDeployment(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dp := s.q().Deployment
	d, err := dp.WithContext(c.Request.Context()).Preload(dp.Nodes).Where(dp.ID.Eq(id)).First()
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	c.JSON(http.StatusOK, d)
}

// ---------- 总览与监控 ----------

type point struct {
	Minute   int64   `json:"minute"`
	Requests uint64  `json:"requests"`
	S5xx     uint64  `json:"s5xx"`
	S4xx     uint64  `json:"s4xx"`
	P95ms    float64 `json:"p95_ms"`
}

func (s *Server) series(groupID uint, minutes int) []point {
	return s.seriesFiltered(groupID, 0, minutes)
}

func (s *Server) seriesFiltered(groupID, nodeID uint, minutes int) []point {
	bucketSec := int64(60)
	switch {
	case minutes > 24*60:
		bucketSec = 3600
	case minutes > 6*60:
		bucketSec = 900
	case minutes > 180:
		bucketSec = 300
	}
	end := time.Now().Truncate(time.Duration(bucketSec) * time.Second).Unix()
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Truncate(time.Duration(bucketSec) * time.Second).Unix()
	type agg struct {
		Minute   int64
		Requests uint64
		S5xx     uint64
		S4xx     uint64
		P95ms    float64
	}
	var rows []agg
	q := s.q()
	m, nd := q.MetricMinute, q.Node
	sel := m.WithContext(bg).Select(m.Minute, m.Requests.Sum().As("requests"), m.S5xx.Sum().As("s5xx"),
		m.S4xx.Sum().As("s4xx"), m.P95ms.Max().As("p95ms")).
		Where(m.Minute.Gte(since)).Group(m.Minute).Order(m.Minute)
	if nodeID != 0 {
		sel = sel.Where(m.NodeID.Eq(nodeID))
	} else if groupID != 0 {
		sel = sel.Where(m.Columns(m.NodeID).In(nd.WithContext(bg).Select(nd.ID).Where(nd.GroupID.Eq(groupID))))
	}
	_ = sel.Scan(&rows)
	idx := map[int64]agg{}
	for _, r := range rows {
		b := (r.Minute / bucketSec) * bucketSec
		cur := idx[b]
		cur.Minute = b
		cur.Requests += r.Requests
		cur.S5xx += r.S5xx
		cur.S4xx += r.S4xx
		if r.P95ms > cur.P95ms {
			cur.P95ms = r.P95ms
		}
		idx[b] = cur
	}
	out := make([]point, 0, int((end-since)/bucketSec)+1)
	for m := since; m < end; m += bucketSec {
		r := idx[m]
		out = append(out, point{Minute: m, Requests: r.Requests, S5xx: r.S5xx, S4xx: r.S4xx, P95ms: r.P95ms})
	}
	return out
}

func (s *Server) metrics(c *gin.Context) {
	minutes, _ := strconv.Atoi(c.DefaultQuery("minutes", "30"))
	if minutes <= 0 || minutes > 7*24*60 {
		minutes = 30
	}
	nodeID, _ := strconv.ParseUint(c.Query("node_id"), 10, 64)
	c.JSON(http.StatusOK, gin.H{"series": s.seriesFiltered(queryGroup(c), uint(nodeID), minutes)})
}

func (s *Server) dashboard(c *gin.Context) {
	q, ctx := s.q(), c.Request.Context()
	nd := q.Node
	nodes, _ := nd.WithContext(ctx).Where(nd.Status.Neq("revoked")).Count()
	online, _ := nd.WithContext(ctx).Where(nd.Status.Eq("online")).Count()
	drifted, _ := nd.WithContext(ctx).Where(nd.Status.Eq("online"), nd.Drifted.Is(true)).Count()
	sites, _ := q.Site.WithContext(ctx).Count()
	groups, _ := q.NodeGroup.WithContext(ctx).Count()
	certExpiring, certExpired := s.Certs.Counts()
	deps, err := q.Deployment.WithContext(ctx).Order(q.Deployment.ID.Desc()).Limit(6).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	ser := s.series(0, 30)
	var req, e5 uint64
	for _, p := range ser[max(0, len(ser)-15):] {
		req += p.Requests
		e5 += p.S5xx
	}
	rate := 0.0
	if req > 0 {
		rate = float64(e5) / float64(req) * 100
	}
	c.JSON(http.StatusOK, gin.H{
		"nodes": nodes, "online": online, "drifted": drifted, "sites": sites, "groups": groups,
		"deployments": deps, "license": s.licView(), "series": ser,
		"requests_15m": req, "error_rate_15m": rate,
		"certs_expiring": certExpiring, "certs_expired": certExpired,
	})
}

// ---------- 审计 ----------

func (s *Server) listAudit(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	a := s.q().AuditLog
	sel := a.WithContext(c.Request.Context()).Order(a.ID.Desc())
	if u := strings.TrimSpace(c.Query("username")); u != "" {
		sel = sel.Where(a.Username.Eq(u))
	}
	if act := strings.TrimSpace(c.Query("action")); act != "" {
		sel = sel.Where(a.Action.Eq(act))
	}
	if res := strings.TrimSpace(c.Query("result")); res != "" {
		sel = sel.Where(a.Result.Eq(res))
	}
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		pat := "%" + kw + "%"
		sel = sel.Where(a.WithContext(c.Request.Context()).Where(a.Username.Like(pat)).Or(a.Action.Like(pat)).Or(a.ObjectID.Like(pat)).Or(a.Detail.Like(pat)))
	}
	rows, err := sel.Offset(offset).Limit(limit).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) verifyAudit(c *gin.Context) {
	bad, err := s.Audit.Verify()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"intact": bad == 0, "first_bad_id": bad})
}

func (s *Server) exportAudit(c *gin.Context) {
	a := s.q().AuditLog
	rows, err := a.WithContext(c.Request.Context()).Order(a.ID).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="audit.csv"`)
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"id", "ts", "username", "ip", "action", "object_type", "object_id", "result", "detail", "hash"})
	for _, r := range rows {
		_ = w.Write([]string{fmt.Sprint(r.ID), r.TS.Format(time.RFC3339), r.Username, r.IP, r.Action, r.ObjectType, r.ObjectID, r.Result, r.Detail, r.Hash})
	}
	w.Flush()
	s.audit(c, "audit.export", "audit", "", nil)
}

// ---------- 用户 ----------

func (s *Server) listUsers(c *gin.Context) {
	uq := s.q().User
	us, err := uq.WithContext(c.Request.Context()).Order(uq.ID).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, us)
}

func (s *Server) createUser(c *gin.Context) {
	var req struct {
		credentials
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if _, ok := rank[req.Role]; !ok {
		fail(c, http.StatusBadRequest, "BAD_ROLE", "unknown role")
		return
	}
	if err := render.ValidateName(req.Username); err != nil {
		fail(c, http.StatusBadRequest, "BAD_USERNAME", "用户名只能包含小写字母、数字和连字符")
		return
	}
	if err := s.policy().CheckPassword(req.Username, req.Password); err != nil {
		fail(c, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
		return
	}
	h, err := crypto.HashPassword(req.Password)
	if err != nil {
		failErr(c, err)
		return
	}
	// 管理员设置的初始口令：首次登录必须修改
	u := &store.User{Username: req.Username, PasswordHash: h, Role: req.Role, Source: "local", MustChangePassword: true}
	if err := s.q().User.WithContext(c.Request.Context()).Create(u); err != nil {
		fail(c, http.StatusConflict, "DUPLICATE", "用户已存在")
		return
	}
	s.audit(c, "user.create", "user", u.Username, map[string]string{"role": u.Role})
	c.JSON(http.StatusOK, u)
}

// pushLicense 把新 License 推送给所有在线节点（agent 侧独立校验）。
func (s *Server) pushLicense() {
	nd := s.q().Node
	ns, _ := nd.WithContext(bg).Where(nd.Status.Eq("online")).Find()
	for _, n := range ns {
		s.GW.PushLicense(n.ID)
	}
}

func auditEntry(c *gin.Context, action, objType, objID, result string) auditE {
	return auditE{Username: c.GetString("user"), IP: c.ClientIP(), Action: action, ObjectType: objType, ObjectID: objID, Result: result}
}
