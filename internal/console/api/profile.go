package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 节点组实例档案（设计 0001 第 3.4、5.2 节）：同机多实例的路径与端口。

type profileView struct {
	render.Profile
	// 生效值（未设置的端口按默认值补齐），供界面显示
	Effective struct {
		HTTPPort   int `json:"http_port"`
		HTTPSPort  int `json:"https_port"`
		StatusPort int `json:"status_port"`
		NoFile     int `json:"nofile"`
	} `json:"effective"`
}

func toProfileView(p render.Profile) profileView {
	v := profileView{Profile: p}
	v.Effective.HTTPPort, v.Effective.HTTPSPort, v.Effective.StatusPort, v.Effective.NoFile = p.HTTP(), p.HTTPS(), p.Status(), p.Files()
	return v
}

func (s *Server) getProfile(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	p, err := s.Deploy.Profile(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toProfileView(p))
}

func (s *Server) setProfile(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var p render.Profile
	if err := c.ShouldBindJSON(&p); err != nil {
		failErr(c, err)
		return
	}
	// 审计由 deploy.SetProfile 写入（含修改前后的档案）
	switch err := s.Deploy.SetProfile(id, p, c.GetString("user"), c.ClientIP()); {
	case err == nil:
		c.JSON(http.StatusOK, toProfileView(p))
	case errors.Is(err, render.ErrProfile):
		fail(c, http.StatusBadRequest, "INVALID_PROFILE", err.Error())
	case errors.Is(err, deploy.ErrProfileLocked):
		fail(c, http.StatusConflict, "PROFILE_LOCKED", err.Error())
	case errors.Is(err, deploy.ErrHostConflict):
		fail(c, http.StatusConflict, "HOST_CONFLICT", err.Error())
	default:
		failErr(c, err)
	}
}
