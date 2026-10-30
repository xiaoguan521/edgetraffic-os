package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/ipsets"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// IP 集 / 地域库（P10 规则积木）：节点组的命名网段集合，被规则的 ip_set 条件引用。

type ipSetView struct {
	store.IPSet
	CIDRs []string `json:"cidrs,omitempty"` // 列表中不返回（地域库可能有上万条），单条查询时返回
	Users []string `json:"users"`           // 引用它的站点 / 规则
}

// ipSetUsers 返回节点组中引用该 IP 集的“站点/规则”。
func (s *Server) ipSetUsers(c *gin.Context, groupID uint, name string) []string {
	q := s.q().Site
	sites, _ := q.WithContext(c.Request.Context()).Where(q.GroupID.Eq(groupID)).Order(q.Name).Find()
	out := []string{}
	for _, st := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) != nil {
			continue
		}
		for _, r := range rs.Rules {
			for _, cd := range r.Conditions {
				if cd.Kind == "ip_set" && cd.Set == name {
					out = append(out, st.Name+"/"+r.Name)
				}
			}
		}
	}
	return out
}

func (s *Server) listIPSets(c *gin.Context) {
	q := s.q().IPSet
	sel := q.WithContext(c.Request.Context()).Omit(q.CIDRs).Order(q.Name)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(q.GroupID.Eq(g))
	}
	rows, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	out := []ipSetView{}
	for _, r := range rows {
		out = append(out, ipSetView{IPSet: *r, Users: s.ipSetUsers(c, r.GroupID, r.Name)})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getIPSet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	r, err := store.First[store.IPSet](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	v := ipSetView{IPSet: *r, Users: s.ipSetUsers(c, r.GroupID, r.Name)}
	_ = json.Unmarshal([]byte(r.CIDRs), &v.CIDRs)
	c.JSON(http.StatusOK, v)
}

type ipSetReq struct {
	GroupID uint     `json:"group_id"`
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	CIDRs   []string `json:"cidrs"`
}

func (s *Server) saveIPSet(c *gin.Context, row *store.IPSet, cidrs []string, source string) {
	norm, err := render.NormalizeCIDRs(cidrs, render.MaxIPSetCIDRs)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_IP_SET", err.Error())
		return
	}
	if len(row.Label) > 128 {
		fail(c, http.StatusBadRequest, "INVALID_IP_SET", "说明不能超过 128 个字符")
		return
	}
	cj, _ := json.Marshal(norm)
	row.CIDRs, row.Count, row.Source, row.UpdatedBy = string(cj), len(norm), source, c.GetString("user")
	q := s.q().IPSet
	action := "ip_set.update"
	if row.ID == 0 {
		if err := render.ValidateName(row.Name); err != nil {
			fail(c, http.StatusBadRequest, "INVALID_IP_SET", "名称只能使用小写字母、数字和连字符")
			return
		}
		if _, err := store.First[store.NodeGroup](s.Store, row.GroupID); err != nil {
			failErr(c, err)
			return
		}
		if n, _ := q.WithContext(c.Request.Context()).Where(q.GroupID.Eq(row.GroupID)).Count(); n >= render.MaxIPSets {
			fail(c, http.StatusBadRequest, "INVALID_IP_SET", fmt.Sprintf("每个节点组最多 %d 个 IP 集", render.MaxIPSets))
			return
		}
		if err := q.WithContext(c.Request.Context()).Create(row); err != nil {
			fail(c, http.StatusConflict, "DUPLICATE", "同名 IP 集已存在")
			return
		}
		action = "ip_set.create"
	} else if err := q.WithContext(c.Request.Context()).Save(row); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, action, "ip_set", row.Name, map[string]any{"group_id": row.GroupID, "count": row.Count, "source": source})
	c.JSON(http.StatusOK, ipSetView{IPSet: *row, Users: s.ipSetUsers(c, row.GroupID, row.Name)})
}

func (s *Server) createIPSet(c *gin.Context) {
	var req ipSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	s.saveIPSet(c, &store.IPSet{GroupID: req.GroupID, Name: strings.TrimSpace(req.Name), Label: strings.TrimSpace(req.Label)}, req.CIDRs, "manual")
}

func (s *Server) updateIPSet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req ipSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	row, err := store.First[store.IPSet](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	row.Label = strings.TrimSpace(req.Label)
	s.saveIPSet(c, row, req.CIDRs, "manual")
}

// importIPSet：multipart 字段 group_id、name、label、country（ISO 两位代码）、file（RIR delegated 统计文件）。
// 同名 IP 集存在时整体替换（用于定期更新地域库）。
func (s *Server) importIPSet(c *gin.Context) {
	var gid uint
	fmt.Sscan(c.PostForm("group_id"), &gid)
	data, ok := readForm(c, "file", 64<<20)
	if !ok {
		return
	}
	country := strings.ToUpper(strings.TrimSpace(c.PostForm("country")))
	cidrs, err := ipsets.ParseRIR(bytes.NewReader(data), country, render.MaxIPSetCIDRs)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_IP_SET", err.Error())
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	q := s.q().IPSet
	row, err := q.WithContext(c.Request.Context()).Where(q.GroupID.Eq(gid), q.Name.Eq(name)).First()
	if err != nil {
		row = &store.IPSet{GroupID: gid, Name: name}
	}
	if l := strings.TrimSpace(c.PostForm("label")); l != "" {
		row.Label = l
	} else if row.Label == "" {
		row.Label = "地域 " + country
	}
	s.saveIPSet(c, row, cidrs, "rir:"+country)
}

func (s *Server) deleteIPSet(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	row, err := store.First[store.IPSet](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if users := s.ipSetUsers(c, row.GroupID, row.Name); len(users) > 0 {
		fail(c, http.StatusConflict, "IN_USE", "IP 集正被规则使用："+strings.Join(users, "、"))
		return
	}
	if _, err := s.q().IPSet.WithContext(c.Request.Context()).Where(s.q().IPSet.ID.Eq(id)).Delete(); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "ip_set.delete", "ip_set", row.Name, map[string]any{"group_id": row.GroupID})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
