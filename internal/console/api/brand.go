package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// brandInfo 返回构建时的品牌名（设计 0001 第 4.4 节）。前端只构建一次，界面上的产品名、数据面名与命令名都从这里取，
// 前端代码中不写死任何品牌名。不需要登录：登录页也要显示产品名，这些名字本来就印在安装包上。
func (s *Server) brandInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"id":       brand.ID(),
		"proc":     brand.Proc(),
		"product":  brand.Product(),
		"fullname": brand.FullName(),
		"agent":    brand.Agent(),
		"lua":      brand.Lua(),
		"instance": brand.ID() + "-instance",
	})
}
