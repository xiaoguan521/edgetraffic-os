package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
)

const maxLegacyUpload = 20 << 20

// Native packages contain trusted executable code, so routes are admin-only
// and a different administrator must approve the immutable frozen version.
func (s *Server) requestLegacy(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxLegacyUpload+(1<<20))
	fh, err := c.FormFile("file")
	if err != nil || fh.Size <= 0 || fh.Size > maxLegacyUpload {
		fail(c, http.StatusBadRequest, "BAD_PACKAGE", "请选择不超过 20 MiB 的 tar.gz 兼容配置包")
		return
	}
	f, err := fh.Open()
	if err != nil {
		failErr(c, err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLegacyUpload+1))
	_ = f.Close()
	if err != nil || len(data) > maxLegacyUpload {
		fail(c, http.StatusBadRequest, "BAD_PACKAGE", "读取配置包失败或文件过大")
		return
	}
	note := strings.TrimSpace(c.PostForm("note"))
	if len(note) > 500 {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "发布说明过长")
		return
	}
	var checks []*agentv1.PostCheck
	if raw := c.PostForm("checks"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &checks); err != nil {
			fail(c, http.StatusBadRequest, "BAD_REQUEST", "探活检查格式无效")
			return
		}
	}
	// Explicit opt-in only: anything but the literal value leaves root masters refused.
	opts := deploy.LegacyOptions{AllowRootMaster: c.PostForm("allow_root_master") == "true"}
	req, err := s.Deploy.RequestLegacy(id, note, data, checks, opts, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, req)
}

func (s *Server) requestLegacyRollback(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		RevisionID string `json:"revision_id"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Note) > 500 {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "回退申请无效")
		return
	}
	req, err := s.Deploy.RequestLegacyRollback(id, body.RevisionID, body.Note, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, req)
}

func (s *Server) approveLegacy(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	rollback, err := s.Deploy.LegacyRequestIsRollback(id)
	if err != nil {
		failErr(c, err)
		return
	}
	if !rollback {
		snap := s.Lic.Snapshot()
		if !snap.Writable() {
			fail(c, http.StatusLocked, "LICENSE_READ_ONLY", "授权状态为 "+string(snap.State)+"，不能批准新的兼容配置")
			return
		}
		if !snap.Feature(license.FeatureLua) {
			fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", "当前授权不包含 Lua 功能")
			return
		}
	}
	var body reviewReq
	_ = c.ShouldBindJSON(&body)
	dep, err := s.Deploy.ApproveLegacy(id, body.Comment, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dep)
}

func (s *Server) reviewLegacyBundle(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	data, err := s.Deploy.LegacyRequestBundle(id, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s-review-%d.tar.gz", brand.ID(), id))
	c.Data(http.StatusOK, "application/gzip", data)
}
