package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
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

// batchPublish 一次发布多个节点组（授权功能 batch_release）：逐组触发发布；开启发布审批时逐组提交申请。
// 某个组没有变更或正忙不影响其他组，结果逐组返回。
func (s *Server) batchPublish(c *gin.Context) {
	var req struct {
		GroupIDs []uint `json:"group_ids"`
		Note     string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if len(req.GroupIDs) == 0 || len(req.GroupIDs) > 100 {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "请选择 1–100 个节点组")
		return
	}
	type result struct {
		GroupID      uint   `json:"group_id"`
		DeploymentID uint   `json:"deployment_id,omitempty"`
		RequestID    uint   `json:"request_id,omitempty"`
		Error        string `json:"error,omitempty"`
	}
	approval := s.Deploy.ApprovalEnabled()
	var out []result
	for _, gid := range req.GroupIDs {
		r := result{GroupID: gid}
		if approval {
			rr, err := s.Deploy.RequestRelease(gid, req.Note, c.GetString("user"), c.ClientIP())
			if err != nil {
				r.Error = err.Error()
			} else {
				r.RequestID = rr.ID
			}
		} else {
			dep, err := s.Deploy.Publish(gid, req.Note, c.GetString("user"), c.ClientIP())
			if err != nil {
				r.Error = err.Error()
			} else {
				r.DeploymentID = dep.ID
			}
		}
		out = append(out, r)
	}
	s.audit(c, "release.batch", "node_group", "batch", map[string]any{"groups": req.GroupIDs, "approval": approval, "results": out})
	c.JSON(http.StatusOK, gin.H{"approval": approval, "results": out})
}

// startCanary 灰度发布（授权功能 canary）。
func (s *Server) startCanary(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Note           string  `json:"note"`
		Percents       []int   `json:"percents"`
		ObserveMinutes int     `json:"observe_minutes"`
		Max5xxPercent  float64 `json:"max_5xx_percent"`
		MinRequests    uint64  `json:"min_requests"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if req.ObserveMinutes < 0 || req.ObserveMinutes > 120 {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "观察时长 0–120 分钟")
		return
	}
	cr, err := s.Deploy.StartCanary(id, req.Note, c.GetString("user"), c.ClientIP(), deploy.CanaryOptions{Percents: req.Percents,
		Observe: time.Duration(req.ObserveMinutes) * time.Minute, Max5xxPercent: req.Max5xxPercent, MinRequests: req.MinRequests})
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, cr)
}

func (s *Server) listCanaries(c *gin.Context) {
	v, err := s.Deploy.ListCanaries(queryGroup(c))
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
