package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
)

// 发布审批（规格书 12.3 P7）。

func (s *Server) approvalGet(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"enabled": s.Deploy.ApprovalEnabled(), "licensed": s.Lic.Snapshot().Feature(license.FeatureReleaseApproval)})
}

// approvalPut：开启需要授权包含 release_approval 且控制台可写；关闭在任何授权状态下都可以（只会放宽流程，不新增能力）。
func (s *Server) approvalPut(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if req.Enabled {
		snap := s.Lic.Snapshot()
		if !snap.Writable() {
			fail(c, http.StatusLocked, "LICENSE_READ_ONLY", "授权状态为 "+string(snap.State)+"，控制台只读："+snap.Reason)
			return
		}
		if !snap.Feature(license.FeatureReleaseApproval) {
			fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", "当前授权不包含功能 "+string(license.FeatureReleaseApproval))
			return
		}
	}
	if err := s.Deploy.SetApproval(req.Enabled, c.GetString("user"), c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	s.approvalGet(c)
}

func (s *Server) requestRelease(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	r, err := s.Deploy.RequestRelease(id, req.Note, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, r)
}

func (s *Server) listReleaseRequests(c *gin.Context) {
	rs, err := s.Deploy.ListRequests(queryGroup(c), c.Query("status"))
	if err != nil {
		failErr(c, err)
		return
	}
	if rs == nil {
		rs = []*store.ReleaseRequest{}
	}
	c.JSON(http.StatusOK, rs)
}

func (s *Server) getReleaseRequest(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	r, err := s.Deploy.GetRequest(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, r)
}

type reviewReq struct {
	Comment string `json:"comment"`
}

func (s *Server) approveRelease(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req reviewReq
	_ = c.ShouldBindJSON(&req)
	dep, err := s.Deploy.Approve(id, req.Comment, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dep)
}

func (s *Server) rejectRelease(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req reviewReq
	_ = c.ShouldBindJSON(&req)
	if err := s.Deploy.Reject(id, req.Comment, c.GetString("user"), c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) cancelRelease(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := s.Deploy.Cancel(id, c.GetString("user"), c.GetString("role") == store.RoleAdmin, c.ClientIP()); err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
