package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

type searchHit struct {
	Kind      string `json:"kind"` // node | site | upstream | cert | stream
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	GroupID   uint   `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	Path      string `json:"path"`
}

// globalSearch 跨全部节点组检索节点、站点/域名、上游/后端 IP、证书和四层代理（⌘K 快速定位）。
func (s *Server) globalSearch(c *gin.Context) {
	kw := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if kw == "" {
		c.JSON(http.StatusOK, gin.H{"hits": []searchHit{}})
		return
	}
	ctx := c.Request.Context()
	q := s.q()
	groups, _ := q.NodeGroup.WithContext(ctx).Select(q.NodeGroup.ID, q.NodeGroup.Name).Find()
	gname := map[uint]string{}
	for _, g := range groups {
		gname[g.ID] = g.Name
	}

	const maxPerKind = 8
	hits := []searchHit{}

	// 1. 节点
	nodes, _ := q.Node.WithContext(ctx).Where(q.Node.Status.Neq("revoked")).Order(q.Node.Name).Find()
	nCount := 0
	for _, n := range nodes {
		if nCount >= maxPerKind {
			break
		}
		if strings.Contains(strings.ToLower(n.Name), kw) ||
			strings.Contains(strings.ToLower(n.RemoteAddr), kw) ||
			strings.Contains(strings.ToLower(n.AppliedRevision), kw) {
			hits = append(hits, searchHit{
				Kind: "node", ID: n.ID, Name: n.Name,
				Detail:  fmt.Sprintf("%s · %s · %s", n.RemoteAddr, n.Status, n.AppliedRevision),
				GroupID: n.GroupID, GroupName: gname[n.GroupID], Path: "/nodes",
			})
			nCount++
		}
	}

	// 2. 站点与域名
	sites, _ := q.Site.WithContext(ctx).Order(q.Site.Name).Find()
	sCount := 0
	for _, st := range sites {
		if sCount >= maxPerKind {
			break
		}
		var rs render.Site
		_ = json.Unmarshal([]byte(st.Spec), &rs)
		domStr := strings.Join(rs.Domains, "，")
		if strings.Contains(strings.ToLower(st.Name), kw) || strings.Contains(strings.ToLower(domStr), kw) {
			hits = append(hits, searchHit{
				Kind: "site", ID: st.ID, Name: st.Name,
				Detail:  domStr,
				GroupID: st.GroupID, GroupName: gname[st.GroupID], Path: "/sites",
			})
			sCount++
		}
	}

	// 3. 上游与后端 IP
	ups, _ := q.Upstream.WithContext(ctx).Order(q.Upstream.Name).Find()
	uCount := 0
	for _, u := range ups {
		if uCount >= maxPerKind {
			break
		}
		var ug render.UpstreamGroup
		_ = json.Unmarshal([]byte(u.Spec), &ug)
		addrs := make([]string, 0, len(ug.Servers))
		for _, srv := range ug.Servers {
			addrs = append(addrs, srv.Addr)
		}
		addrStr := strings.Join(addrs, "，")
		if strings.Contains(strings.ToLower(u.Name), kw) || strings.Contains(strings.ToLower(addrStr), kw) {
			hits = append(hits, searchHit{
				Kind: "upstream", ID: u.ID, Name: u.Name,
				Detail:  addrStr,
				GroupID: u.GroupID, GroupName: gname[u.GroupID], Path: "/sites?tab=ups",
			})
			uCount++
		}
	}

	// 4. 证书
	certs, _ := q.Certificate.WithContext(ctx).Order(q.Certificate.Name).Find()
	cCount := 0
	for _, ct := range certs {
		if cCount >= maxPerKind {
			break
		}
		var doms []string
		_ = json.Unmarshal([]byte(ct.Domains), &doms)
		domStr := strings.Join(doms, "，")
		if strings.Contains(strings.ToLower(ct.Name), kw) || strings.Contains(strings.ToLower(domStr), kw) {
			hits = append(hits, searchHit{
				Kind: "cert", ID: ct.ID, Name: ct.Name,
				Detail: domStr, Path: "/certificates",
			})
			cCount++
		}
	}

	// 5. 四层代理
	streams, _ := q.StreamService.WithContext(ctx).Order(q.StreamService.Name).Find()
	stCount := 0
	for _, sm := range streams {
		if stCount >= maxPerKind {
			break
		}
		var sp render.StreamProxy
		_ = json.Unmarshal([]byte(sm.Spec), &sp)
		addrs := make([]string, 0, len(sp.Servers))
		for _, srv := range sp.Servers {
			addrs = append(addrs, srv.Addr)
		}
		listen := fmt.Sprintf("%s/%d", sp.Protocol, sp.Listen)
		detail := listen + " → " + strings.Join(addrs, "，")
		if strings.Contains(strings.ToLower(sm.Name), kw) || strings.Contains(strings.ToLower(detail), kw) {
			hits = append(hits, searchHit{
				Kind: "stream", ID: sm.ID, Name: sm.Name,
				Detail:  detail,
				GroupID: sm.GroupID, GroupName: gname[sm.GroupID], Path: "/streams",
			})
			stCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{"hits": hits})
}
