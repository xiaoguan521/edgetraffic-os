package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/crypto"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

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
	s.Store.DB.Model(&store.Node{}).Where("status <> ?", "revoked").Count(&v.NodesUsed)
	s.Store.DB.Model(&store.Site{}).Count(&v.SitesUsed)
	return v
}

func (s *Server) userCount() int64 {
	var n int64
	s.Store.DB.Model(&store.User{}).Count(&n)
	return n
}

func (s *Server) setupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"initialized": s.userCount() > 0, "license": s.licView()})
}

type credentials struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

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
	h, err := crypto.HashPassword(req.Password)
	if err != nil {
		failErr(c, err)
		return
	}
	u := &store.User{Username: req.Username, PasswordHash: h, Role: store.RoleAdmin}
	if err := s.Store.DB.Create(u).Error; err != nil {
		failErr(c, err)
		return
	}
	// 初始化默认节点组
	var n int64
	s.Store.DB.Model(&store.NodeGroup{}).Count(&n)
	if n == 0 {
		s.Store.DB.Create(&store.NodeGroup{Name: "default", Description: "默认节点组", AllowedCIDRs: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"})
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
	var u store.User
	ok := s.Store.DB.Where("username = ?", req.Username).First(&u).Error == nil && crypto.CheckPassword(u.PasswordHash, req.Password)
	if !ok {
		lf.mu.Lock()
		if lf.n++; lf.n >= 5 {
			lf.n, lf.until = 0, time.Now().Add(15*time.Minute)
		}
		lf.mu.Unlock()
		c.Set("user", req.Username)
		_ = s.Audit.Write(auditEntry(c, "auth.login", "user", req.Username, "failure"))
		fail(c, http.StatusUnauthorized, "BAD_CREDENTIALS", "用户名或密码错误")
		return
	}
	lf.mu.Lock()
	lf.n = 0
	lf.mu.Unlock()
	now := time.Now()
	s.Store.DB.Model(&u).Update("last_login_at", now)
	c.Set("user", u.Username)
	s.audit(c, "auth.login", "user", u.Username, nil)
	if err := s.issue(c, &u); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"username": u.Username, "role": u.Role})
}

func (s *Server) logout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode})
	c.Status(http.StatusNoContent)
}

func (s *Server) me(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"username": c.GetString("user"), "role": c.GetString("role")})
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
	s.audit(c, "license.import", "license", id, map[string]string{"state": string(snap.State)})
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
	var gs []store.NodeGroup
	s.Store.DB.Order("id").Find(&gs)
	type row struct {
		store.NodeGroup
		Nodes  int64 `json:"nodes"`
		Online int64 `json:"online"`
		Sites  int64 `json:"sites"`
	}
	out := make([]row, 0, len(gs))
	for _, g := range gs {
		r := row{NodeGroup: g}
		s.Store.DB.Model(&store.Node{}).Where("group_id = ? AND status <> ?", g.ID, "revoked").Count(&r.Nodes)
		s.Store.DB.Model(&store.Node{}).Where("group_id = ? AND status = ?", g.ID, "online").Count(&r.Online)
		s.Store.DB.Model(&store.Site{}).Where("group_id = ?", g.ID).Count(&r.Sites)
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
	g := &store.NodeGroup{Name: req.Name, Description: req.Description, AllowedCIDRs: req.AllowedCIDRs, RealIPFrom: req.RealIPFrom}
	if err := s.Store.DB.Create(g).Error; err != nil {
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
	g, err := store.First[store.NodeGroup](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if err := s.Store.DB.Model(g).Updates(map[string]any{"description": req.Description, "allowed_cidrs": req.AllowedCIDRs, "real_ip_from": req.RealIPFrom}).Error; err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "group.update", "node_group", fmt.Sprint(id), req)
	c.JSON(http.StatusOK, g)
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
		var n int64
		s.Store.DB.Model(&store.Node{}).Where("status <> ?", "revoked").Count(&n)
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
	cmd := fmt.Sprintf("sudo syapp-agent enroll -server %s -token %s -ca-fingerprint %s && sudo systemctl enable --now syapp-agent", s.GRPCAddr, tok, s.CAFingerprint)
	c.JSON(http.StatusOK, gin.H{"token": tok, "expires_at": exp, "server": s.GRPCAddr, "ca_fingerprint": s.CAFingerprint, "command": cmd})
}

// ---------- 节点 ----------

func (s *Server) listNodes(c *gin.Context) {
	var ns []store.Node
	q := s.Store.DB.Order("id")
	if g := queryGroup(c); g != 0 {
		q = q.Where("group_id = ?", g)
	}
	q.Find(&ns)
	c.JSON(http.StatusOK, ns)
}

func (s *Server) revokeNode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	s.Store.DB.Model(&store.Node{}).Where("id = ?", id).Update("status", "revoked")
	s.GW.Disconnect(id)
	s.audit(c, "node.revoke", "node", fmt.Sprint(id), nil)
	c.Status(http.StatusNoContent)
}

// ---------- 站点 ----------

type siteView struct {
	store.Site
	Spec render.Site `json:"spec"`
}

func toSiteView(st store.Site) siteView {
	v := siteView{Site: st}
	_ = json.Unmarshal([]byte(st.Spec), &v.Spec)
	v.Spec.Name = st.Name
	if v.Spec.HTTPS != nil { // 私钥不回显（规格书 9.3）
		v.Spec.HTTPS.KeyPEM = ""
	}
	return v
}

func (s *Server) listSites(c *gin.Context) {
	var ss []store.Site
	q := s.Store.DB.Order("name")
	if g := queryGroup(c); g != 0 {
		q = q.Where("group_id = ?", g)
	}
	q.Find(&ss)
	out := make([]siteView, 0, len(ss))
	for _, st := range ss {
		out = append(out, toSiteView(st))
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
			keep = append(keep, *site)
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
		var n int64
		s.Store.DB.Model(&store.Site{}).Count(&n)
		if n >= int64(snap.Payload.Limits.Sites) {
			fail(c, http.StatusPaymentRequired, "LICENSE_SITE_LIMIT", fmt.Sprintf("已达到授权站点数上限（%d）", snap.Payload.Limits.Sites))
			return
		}
	}
	if err := s.validateWith(req.GroupID, &req.Spec, nil, "", ""); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_SITE", err.Error())
		return
	}
	spec, _ := json.Marshal(req.Spec)
	st := &store.Site{GroupID: req.GroupID, Name: req.Spec.Name, Spec: string(spec), Enabled: req.Enabled == nil || *req.Enabled, Version: 1, UpdatedBy: c.GetString("user")}
	if err := s.Store.DB.Create(st).Error; err != nil {
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
	if req.Spec.HTTPS != nil && req.Spec.HTTPS.KeyPEM == "" { // 未重新上传私钥时沿用原私钥
		var old render.Site
		_ = json.Unmarshal([]byte(st.Spec), &old)
		if old.HTTPS != nil {
			req.Spec.HTTPS.KeyPEM = old.HTTPS.KeyPEM
		}
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
	res := s.Store.DB.Model(&store.Site{}).Where("id = ? AND version = ?", id, st.Version).
		Updates(map[string]any{"spec": string(spec), "enabled": enabled, "version": st.Version + 1, "updated_by": c.GetString("user")})
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
	s.Store.DB.Delete(st)
	s.audit(c, "site.delete", "site", st.Name, nil)
	c.Status(http.StatusNoContent)
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
	var us []store.Upstream
	q := s.Store.DB.Order("name")
	if g := queryGroup(c); g != 0 {
		q = q.Where("group_id = ?", g)
	}
	q.Find(&us)
	out := make([]upstreamView, 0, len(us))
	for _, u := range us {
		v := upstreamView{Upstream: u}
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
	if err := s.Store.DB.Create(u).Error; err != nil {
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
	s.Store.DB.Model(u).Updates(map[string]any{"spec": string(spec), "updated_by": c.GetString("user")})
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
	s.Store.DB.Delete(u)
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
	c.JSON(http.StatusOK, gin.H{"files": diffs, "digest": digest, "changed": len(diffs) > 0})
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
	var rs []store.Revision
	q := s.Store.DB.Select("seq, id, group_id, parent_id, digest, note, created_by, created_at").Order("seq desc").Limit(50)
	if g := queryGroup(c); g != 0 {
		q = q.Where("group_id = ?", g)
	}
	q.Find(&rs)
	c.JSON(http.StatusOK, rs)
}

func (s *Server) listDeployments(c *gin.Context) {
	var ds []store.Deployment
	q := s.Store.DB.Order("id desc").Limit(30)
	if g := queryGroup(c); g != 0 {
		q = q.Where("group_id = ?", g)
	}
	q.Preload("Nodes").Find(&ds)
	c.JSON(http.StatusOK, ds)
}

func (s *Server) getDeployment(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var d store.Deployment
	if err := s.Store.DB.Preload("Nodes").First(&d, id).Error; err != nil {
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
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Truncate(time.Minute).Unix()
	type agg struct {
		Minute   int64
		Requests uint64
		S5xx     uint64
		S4xx     uint64
		P95ms    float64
	}
	var rows []agg
	q := s.Store.DB.Model(&store.MetricMinute{}).Select("minute, sum(requests) as requests, sum(s5xx) as s5xx, sum(s4xx) as s4xx, max(p95ms) as p95ms").
		Where("minute >= ?", since).Group("minute").Order("minute")
	if groupID != 0 {
		q = q.Where("node_id IN (?)", s.Store.DB.Model(&store.Node{}).Select("id").Where("group_id = ?", groupID))
	}
	q.Scan(&rows)
	idx := map[int64]agg{}
	for _, r := range rows {
		idx[r.Minute] = r
	}
	out := make([]point, 0, minutes)
	end := time.Now().Truncate(time.Minute).Unix()
	for m := since; m < end; m += 60 {
		r := idx[m]
		out = append(out, point{Minute: m, Requests: r.Requests, S5xx: r.S5xx, S4xx: r.S4xx, P95ms: r.P95ms})
	}
	return out
}

func (s *Server) metrics(c *gin.Context) {
	minutes, _ := strconv.Atoi(c.DefaultQuery("minutes", "30"))
	if minutes <= 0 || minutes > 24*60 {
		minutes = 30
	}
	c.JSON(http.StatusOK, gin.H{"series": s.series(queryGroup(c), minutes)})
}

func (s *Server) dashboard(c *gin.Context) {
	var nodes, online, drifted, sites, groups int64
	s.Store.DB.Model(&store.Node{}).Where("status <> ?", "revoked").Count(&nodes)
	s.Store.DB.Model(&store.Node{}).Where("status = ?", "online").Count(&online)
	s.Store.DB.Model(&store.Node{}).Where("status = ? AND drifted = ?", "online", true).Count(&drifted)
	s.Store.DB.Model(&store.Site{}).Count(&sites)
	s.Store.DB.Model(&store.NodeGroup{}).Count(&groups)
	var deps []store.Deployment
	s.Store.DB.Order("id desc").Limit(6).Find(&deps)
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
	})
}

// ---------- 审计 ----------

func (s *Server) listAudit(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []store.AuditLog
	s.Store.DB.Order("id desc").Limit(limit).Find(&rows)
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
	var rows []store.AuditLog
	s.Store.DB.Order("id").Find(&rows)
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
	var us []store.User
	s.Store.DB.Order("id").Find(&us)
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
	h, err := crypto.HashPassword(req.Password)
	if err != nil {
		failErr(c, err)
		return
	}
	u := &store.User{Username: req.Username, PasswordHash: h, Role: req.Role}
	if err := s.Store.DB.Create(u).Error; err != nil {
		fail(c, http.StatusConflict, "DUPLICATE", "用户已存在")
		return
	}
	s.audit(c, "user.create", "user", u.Username, map[string]string{"role": u.Role})
	c.JSON(http.StatusOK, u)
}

// pushLicense 把新 License 推送给所有在线节点（agent 侧独立校验）。
func (s *Server) pushLicense() {
	var ns []store.Node
	s.Store.DB.Where("status = ?", "online").Find(&ns)
	for _, n := range ns {
		s.GW.PushLicense(n.ID)
	}
}

func auditEntry(c *gin.Context, action, objType, objID, result string) auditE {
	return auditE{Username: c.GetString("user"), IP: c.ClientIP(), Action: action, ObjectType: objType, ObjectID: objID, Result: result}
}
