package api

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 四层代理（P9，授权功能 stream）。

type streamView struct {
	store.StreamService
	Spec render.StreamProxy `json:"spec"`
}

func toStreamView(st store.StreamService) streamView {
	v := streamView{StreamService: st}
	_ = json.Unmarshal([]byte(st.Spec), &v.Spec)
	v.Spec.Name = st.Name
	return v
}

func (s *Server) listStreams(c *gin.Context) {
	q := s.q().StreamService
	sel := q.WithContext(c.Request.Context()).Order(q.Name)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(q.GroupID.Eq(g))
	}
	rows, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	out := []streamView{}
	for _, r := range rows {
		out = append(out, toStreamView(*r))
	}
	c.JSON(http.StatusOK, out)
}

type streamReq struct {
	GroupID uint               `json:"group_id"`
	Spec    render.StreamProxy `json:"spec"`
	Enabled *bool              `json:"enabled"`
}

// validateStream 把新服务放进节点组的完整输入里校验（端口冲突、回源网段等）。
func (s *Server) validateStream(groupID uint, sp render.StreamProxy) error {
	in, err := s.Deploy.Input(groupID, "r000000")
	if err != nil {
		return err
	}
	var keep []render.StreamProxy
	for _, x := range in.Streams {
		if x.Name != sp.Name {
			keep = append(keep, x)
		}
	}
	in.Streams = append(keep, sp)
	if len(in.MimeTypes) == 0 {
		in.MimeTypes = render.DefaultMimeTypes()
	}
	if _, err = render.Render(in); err != nil {
		return err
	}
	// 同一主机上其他实例的端口（同组内的冲突已由渲染器检查）
	return s.Deploy.CheckStreamPorts(groupID, sp)
}

func (s *Server) saveStream(c *gin.Context) {
	var req streamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	var row *store.StreamService
	q := s.q().StreamService
	if c.Param("id") != "" {
		id, ok := parseID(c)
		if !ok {
			return
		}
		r, err := q.WithContext(c.Request.Context()).Where(q.ID.Eq(id)).First()
		if err != nil {
			failErr(c, store.ErrNotFound)
			return
		}
		row = r
		req.GroupID, req.Spec.Name = r.GroupID, r.Name
	} else {
		if _, err := store.First[store.NodeGroup](s.Store, req.GroupID); err != nil {
			failErr(c, err)
			return
		}
		row = &store.StreamService{GroupID: req.GroupID, Name: req.Spec.Name, Enabled: true}
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if row.Enabled {
		if err := s.validateStream(req.GroupID, req.Spec); err != nil {
			fail(c, http.StatusBadRequest, "INVALID_STREAM", err.Error())
			return
		}
	} else if err := render.ValidateName(req.Spec.Name); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_STREAM", err.Error())
		return
	}
	spec, _ := json.Marshal(req.Spec)
	row.Spec, row.UpdatedBy = string(spec), c.GetString("user")
	if err := q.WithContext(c.Request.Context()).Save(row); err != nil {
		fail(c, http.StatusConflict, "DUPLICATE", "同名四层代理已存在")
		return
	}
	s.audit(c, "stream.save", "stream", row.Name, req.Spec)
	c.JSON(http.StatusOK, toStreamView(*row))
}

func (s *Server) deleteStream(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	q := s.q().StreamService
	r, err := q.WithContext(c.Request.Context()).Where(q.ID.Eq(id)).First()
	if err != nil {
		failErr(c, store.ErrNotFound)
		return
	}
	if _, err := q.WithContext(c.Request.Context()).Where(q.ID.Eq(id)).Delete(); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "stream.delete", "stream", r.Name, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
