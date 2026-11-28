package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
)

// badges 返回侧边栏与顶栏使用的全局待办/异常角标统计。
func (s *Server) badges(c *gin.Context) {
	q, ctx := s.q(), c.Request.Context()
	pendingReleases, _ := q.ReleaseRequest.WithContext(ctx).Where(q.ReleaseRequest.Status.Eq(deploy.ApprovalPending)).Count()
	pendingSnippets, _ := q.Snippet.WithContext(ctx).Where(q.Snippet.PendingBy.Neq("")).Count()
	firingAlerts, _ := q.AlertEvent.WithContext(ctx).Where(q.AlertEvent.Status.Eq("firing")).Count()
	driftedNodes, _ := q.Node.WithContext(ctx).Where(q.Node.Status.Eq("online"), q.Node.Drifted.Is(true)).Count()
	offlineNodes, _ := q.Node.WithContext(ctx).Where(q.Node.Status.Eq("offline")).Count()
	c.JSON(http.StatusOK, gin.H{
		"pending_releases": pendingReleases,
		"pending_snippets": pendingSnippets,
		"firing_alerts":    firingAlerts,
		"drifted_nodes":    driftedNodes,
		"offline_nodes":    offlineNodes,
	})
}

func (s *Server) revisionFiles(c *gin.Context) {
	gid := queryGroup(c)
	if gid == 0 {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "缺少 group_id")
		return
	}
	revID := c.Param("id")
	files, legacy, err := s.Deploy.RevisionFiles(gid, revID)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"revision_id": revID, "legacy": legacy, "files": files})
}

func (s *Server) diffRevisions(c *gin.Context) {
	gid := queryGroup(c)
	fromID := c.Query("from")
	toID := c.Query("to")
	if gid == 0 || fromID == "" || toID == "" {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "缺少 group_id、from 或 to 参数")
		return
	}
	diffs, err := s.Deploy.CompareRevisions(gid, fromID, toID)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": fromID, "to": toID, "files": diffs})
}
