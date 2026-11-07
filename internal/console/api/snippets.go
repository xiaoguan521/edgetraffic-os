package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 自定义片段（规格书 12.7 路径 A）。

func (s *Server) listSnippets(c *gin.Context) {
	rows, err := s.Deploy.ListSnippets(queryGroup(c))
	if err != nil {
		failErr(c, err)
		return
	}
	if rows == nil {
		rows = []*store.Snippet{}
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) snippetStats(c *gin.Context) {
	st, err := s.Deploy.SnippetStats()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// luaAdmin：自定义 Lua 脚本（插入点 lua）的提交、校验、审批、驳回、删除与撤回都只允许管理员（另一位管理员批准）。
// 已写入 403 响应时返回 false。
func luaAdmin(c *gin.Context, scope string) bool {
	if scope == string(render.ScopeLua) && rank[c.GetString("role")] < rank[store.RoleAdmin] {
		fail(c, http.StatusForbidden, "FORBIDDEN", "自定义 Lua 脚本只有管理员可以提交与审批")
		return false
	}
	return true
}

// luaAdminByID 按片段编号检查（审批、驳回、删除、撤回）。
func (s *Server) luaAdminByID(c *gin.Context, id uint) bool {
	row, err := s.Deploy.GetSnippet(id)
	if err != nil {
		failErr(c, err)
		return false
	}
	return luaAdmin(c, row.Scope)
}

// snippetFail 把提交 / 校验的错误写成响应：授权不含 lua 时为 402。
func snippetFail(c *gin.Context, err error) {
	if errors.Is(err, deploy.ErrLuaScriptNotLicensed) {
		fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", err.Error())
		return
	}
	fail(c, http.StatusBadRequest, "SNIPPET_INVALID", err.Error())
}

func snippetIssues(c *gin.Context, issues []render.SnippetIssue) {
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"code": "SNIPPET_REJECTED", "message": "片段包含不允许的指令", "issues": issues})
}

// validateSnippet 只校验不保存（编辑器实时检查用）。
func (s *Server) validateSnippet(c *gin.Context) {
	var in deploy.SnippetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		failErr(c, err)
		return
	}
	if !luaAdmin(c, in.Scope) {
		return
	}
	norm, issues, err := s.Deploy.CheckSnippet(in)
	if err != nil {
		snippetFail(c, err)
		return
	}
	if issues == nil {
		issues = []render.SnippetIssue{}
	}
	c.JSON(http.StatusOK, gin.H{"ok": len(issues) == 0, "normalized": norm, "issues": issues})
}

func (s *Server) submitSnippet(c *gin.Context) {
	var in deploy.SnippetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		failErr(c, err)
		return
	}
	if !luaAdmin(c, in.Scope) {
		return
	}
	row, issues, err := s.Deploy.SubmitSnippet(in, c.GetString("user"), c.ClientIP())
	if err != nil {
		snippetFail(c, err)
		return
	}
	if len(issues) > 0 {
		snippetIssues(c, issues)
		return
	}
	c.JSON(http.StatusOK, row)
}

func (s *Server) deleteSnippet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if !s.luaAdminByID(c, id) {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	if err := s.Deploy.SubmitSnippetDelete(id, c.GetString("user"), req.Note, c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) approveSnippet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if !s.luaAdminByID(c, id) {
		return
	}
	if err := s.Deploy.ApproveSnippet(id, c.GetString("user"), c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) rejectSnippet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if !s.luaAdminByID(c, id) {
		return
	}
	var req reviewReq
	_ = c.ShouldBindJSON(&req)
	if err := s.Deploy.RejectSnippet(id, c.GetString("user"), req.Comment, c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) withdrawSnippet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if !s.luaAdminByID(c, id) {
		return
	}
	if err := s.Deploy.WithdrawSnippet(id, c.GetString("user"), c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// nodeDrift 返回节点漂移的逐文件差异（规格书 12.7 路径 B）。
func (s *Server) nodeDrift(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	v, err := s.Deploy.DriftView(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// adoptDrift 把节点上的手工改动转成待审批的片段。
func (s *Server) adoptDrift(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	r, err := s.Deploy.AdoptDrift(id, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, r)
}
