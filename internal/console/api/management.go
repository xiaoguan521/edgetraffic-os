package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
)

func (s *Server) getManagementMode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	view, err := s.Deploy.ManagementModeView(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (s *Server) setManagementMode(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if err := s.Deploy.SetManagementMode(id, req.Mode, c.GetString("user"), c.ClientIP()); err != nil {
		if errors.Is(err, deploy.ErrInvalidManagementMode) {
			fail(c, http.StatusBadRequest, "INVALID_MANAGEMENT_MODE", "模式只能是 managed、observe 或 legacy")
			return
		}
		failErr(c, err)
		return
	}
	view, err := s.Deploy.ManagementModeView(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}
