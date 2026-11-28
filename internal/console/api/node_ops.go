package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// updateNode 修改节点显示名或将节点迁移到实例档案一致的另一个节点组。
func (s *Server) updateNode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Name    *string `json:"name"`
		GroupID *uint   `json:"group_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	n, err := store.First[store.Node](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if n.Status == "revoked" {
		fail(c, http.StatusBadRequest, "NODE_REVOKED", "已吊销的节点不能修改")
		return
	}
	changes := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 64 || strings.ContainsAny(name, "\r\n\t") {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "节点名称需为 1–64 个有效字符")
			return
		}
		if name != n.Name {
			changes["old_name"] = n.Name
			changes["name"] = name
			n.Name = name
		}
	}
	if req.GroupID != nil && *req.GroupID != n.GroupID {
		targetID := *req.GroupID
		if _, err := store.First[store.NodeGroup](s.Store, targetID); err != nil {
			failErr(c, err)
			return
		}
		unlock, locked := s.Deploy.TryLockGroups(n.GroupID, targetID)
		if !locked {
			fail(c, http.StatusConflict, "BUSY", "源节点组或目标节点组正在执行发布或升级，请完成后再迁移节点")
			return
		}
		defer unlock()
		srcProf, err := s.Deploy.Profile(n.GroupID)
		if err != nil {
			failErr(c, err)
			return
		}
		dstProf, err := s.Deploy.Profile(targetID)
		if err != nil {
			failErr(c, err)
			return
		}
		modeOf := func(m string) string {
			if m == "" {
				return "system"
			}
			return m
		}
		resOf := func(m string) string {
			if m == "" {
				return "shared"
			}
			return m
		}
		if modeOf(srcProf.Mode) != modeOf(dstProf.Mode) ||
			srcProf.Instance != dstProf.Instance ||
			srcProf.Root != dstProf.Root ||
			resOf(srcProf.ResourceMode) != resOf(dstProf.ResourceMode) ||
			srcProf.NoFile != dstProf.NoFile ||
			srcProf.HTTP() != dstProf.HTTP() ||
			srcProf.HTTPS() != dstProf.HTTPS() ||
			srcProf.Status() != dstProf.Status() {
			fail(c, http.StatusConflict, "PROFILE_MISMATCH", "源节点组与目标节点组的实例档案（运行模式、实例名、根目录、共享方式、文件句柄上限或端口）不一致，无法直接迁移节点")
			return
		}
		changes["old_group_id"] = n.GroupID
		changes["group_id"] = targetID
		n.GroupID = targetID
	}
	if len(changes) == 0 {
		c.JSON(http.StatusOK, n)
		return
	}
	if err := s.q().Node.WithContext(c.Request.Context()).Save(n); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "node.update", "node", fmt.Sprint(n.ID), changes)
	c.JSON(http.StatusOK, n)
}

func (s *Server) syncNode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	allowLegacy := c.GetString("role") == store.RoleAdmin
	dep, err := s.Deploy.SyncNode(id, c.GetString("user"), allowLegacy, c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dep)
}
