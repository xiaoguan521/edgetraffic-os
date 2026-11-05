package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 静态资源发布（P9）。

func (s *Server) listStatic(c *gin.Context) {
	sb := s.q().StaticBundle
	sel := sb.WithContext(c.Request.Context()).Select(sb.ID, sb.GroupID, sb.Name, sb.SHA256, sb.Size, sb.Files, sb.UploadedBy, sb.CreatedAt).Order(sb.ID.Desc())
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(sb.GroupID.Eq(g))
	}
	rows, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	if rows == nil {
		rows = []*store.StaticBundle{}
	}
	c.JSON(http.StatusOK, rows)
}

// uploadStatic：multipart 字段 group_id、name、file（.tar.gz 或 .zip）。
func (s *Server) uploadStatic(c *gin.Context) {
	var gid uint
	_, _ = fmt.Sscan(c.PostForm("group_id"), &gid)
	data, ok := readForm(c, "file", 64<<20)
	if !ok {
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" || len(name) > 128 {
		fh, _ := c.FormFile("file")
		name = fh.Filename
	}
	// allow_no_index=1：没有根目录 index.html 的纯资源包（下载目录、图片 / 脚本等）
	opt := deploy.StaticImportOptions{AllowNoIndex: c.PostForm("allow_no_index") == "1"}
	row, err := s.Deploy.ImportStaticWith(gid, name, data, opt, c.GetString("user"), c.ClientIP())
	if err != nil {
		code := "STATIC_REJECTED"
		if deploy.IsStaticNoIndex(err) {
			code = "STATIC_NO_INDEX"
		}
		fail(c, http.StatusBadRequest, code, err.Error())
		return
	}
	row.Data = nil
	c.JSON(http.StatusOK, row)
}

func (s *Server) deleteStatic(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	// 被站点引用的资源包不能删除（回滚到旧版本时仍在版本包中，不受影响）
	sites, err := s.q().Site.WithContext(c.Request.Context()).Find()
	if err != nil {
		failErr(c, err)
		return
	}
	for _, st := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) == nil {
			for _, r := range rs.Routes {
				if r.Static != nil && r.Static.BundleID == id {
					fail(c, http.StatusConflict, "IN_USE", "站点 "+st.Name+" 的路由 "+r.Path+" 正在使用该资源包")
					return
				}
			}
		}
	}
	sb := s.q().StaticBundle
	if _, err := sb.WithContext(c.Request.Context()).Where(sb.ID.Eq(id)).Delete(); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "static.delete", "static_bundle", fmt.Sprint(id), nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
