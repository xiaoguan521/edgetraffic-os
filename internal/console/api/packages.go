package api

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// 数据面版本库与分批热升级（规格书 12.6）。

func (s *Server) listPackages(c *gin.Context) {
	rows, err := s.Deploy.ListPackages()
	if err != nil {
		failErr(c, err)
		return
	}
	if rows == nil {
		rows = []*store.DataplanePackage{}
	}
	c.JSON(http.StatusOK, rows)
}

func readForm(c *gin.Context, field string, limit int64) ([]byte, bool) {
	fh, err := c.FormFile(field)
	if err != nil {
		fail(c, http.StatusBadRequest, "NO_FILE", "缺少文件 "+field)
		return nil, false
	}
	if fh.Size > limit {
		fail(c, http.StatusRequestEntityTooLarge, "TOO_LARGE", field+" 超过大小上限")
		return nil, false
	}
	f, err := fh.Open()
	if err != nil {
		failErr(c, err)
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		failErr(c, err)
		return nil, false
	}
	return b, true
}

// importPackage：multipart 字段 package（tar.gz）与 signature（.sig）。
func (s *Server) importPackage(c *gin.Context) {
	data, ok := readForm(c, "package", 128<<20)
	if !ok {
		return
	}
	sig, ok := readForm(c, "signature", 64<<10)
	if !ok {
		return
	}
	p, err := s.Deploy.ImportPackage(data, sig, c.GetString("user"), c.ClientIP())
	if err != nil {
		fail(c, http.StatusBadRequest, "PACKAGE_REJECTED", err.Error())
		return
	}
	c.JSON(http.StatusOK, p)
}

func (s *Server) startUpgrade(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Version string `json:"version"`
		Variant string `json:"variant"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if req.Variant == "" {
		req.Variant = "standard"
	}
	up, err := s.Deploy.StartUpgrade(id, req.Version, req.Variant, c.GetString("user"), c.ClientIP())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, up)
}

func (s *Server) listUpgrades(c *gin.Context) {
	v, err := s.Deploy.ListUpgrades(queryGroup(c))
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (s *Server) getUpgrade(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	v, err := s.Deploy.GetUpgrade(id)
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
