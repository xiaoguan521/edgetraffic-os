package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

type copyGroupReq struct {
	SourceGroupID    uint   `json:"source_group_id"`
	IncludeUpstreams bool   `json:"include_upstreams"`
	IncludeIPSets    bool   `json:"include_ip_sets"`
	IncludeSites     bool   `json:"include_sites"`
	IncludeStreams   bool   `json:"include_streams"`
	SiteIDs          []uint `json:"site_ids"`  // 可选：只复制指定站点，并自动携带其依赖的上游与组内 IP 集
	Overwrite        bool   `json:"overwrite"` // 同名项是否覆盖（默认 false 跳过）
}

type copyCount struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

func parseAllowedPrefixes(raw string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// copyGroupConfig 将源节点组的草稿配置（上游、组内 IP 集、站点、四层代理）复制到目标节点组。
// 不自动复制自定义片段/Lua 脚本（须走双人审批），且结果仅保存为草稿，需在目标组预览并发布后生效。
func (s *Server) copyGroupConfig(c *gin.Context) {
	targetID, ok := parseID(c)
	if !ok {
		return
	}
	var req copyGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	if req.SourceGroupID == 0 || req.SourceGroupID == targetID {
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "请选择与目标不同的源节点组")
		return
	}
	targetGroup, err := store.First[store.NodeGroup](s.Store, targetID)
	if err != nil {
		failErr(c, err)
		return
	}
	sourceGroup, err := store.First[store.NodeGroup](s.Store, req.SourceGroupID)
	if err != nil {
		failErr(c, err)
		return
	}

	ctx := c.Request.Context()
	q := s.q()
	user := c.GetString("user")
	warnings := []string{}

	// 1. 确定要复制的站点与依赖集合
	var srcSites []*store.Site
	needUpstreams := map[string]bool{}
	needIPSets := map[string]bool{}
	selectedMode := len(req.SiteIDs) > 0
	if selectedMode {
		req.IncludeSites = true
		srcSites, err = q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(req.SourceGroupID), q.Site.ID.In(req.SiteIDs...)).Order(q.Site.Name).Find()
		if err != nil {
			failErr(c, err)
			return
		}
	} else if req.IncludeSites {
		srcSites, err = q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(req.SourceGroupID)).Order(q.Site.Name).Find()
		if err != nil {
			failErr(c, err)
			return
		}
	}
	for _, st := range srcSites {
		var rs render.Site
		if json.Unmarshal([]byte(st.Spec), &rs) != nil {
			continue
		}
		for _, r := range rs.Routes {
			if r.Mode == "proxy" && r.Proxy != nil && r.Proxy.Upstream != "" {
				needUpstreams[r.Proxy.Upstream] = true
			}
		}
		for _, rl := range rs.Rules {
			if rl.Action.Type == "auth_request" && rl.Action.Auth != nil && rl.Action.Auth.Upstream != "" {
				needUpstreams[rl.Action.Auth.Upstream] = true
			}
			for _, cd := range rl.Conditions {
				if cd.Kind == "ip_set" && cd.Set != "" {
					needIPSets[cd.Set] = true
				}
			}
		}
	}

	// 2. 校验授权配额与功能门禁
	var srcStreams []*store.StreamService
	if req.IncludeStreams && !selectedMode {
		srcStreams, err = q.StreamService.WithContext(ctx).Where(q.StreamService.GroupID.Eq(req.SourceGroupID)).Order(q.StreamService.Name).Find()
		if err != nil {
			failErr(c, err)
			return
		}
		if len(srcStreams) > 0 && !s.Lic.Snapshot().Feature(license.FeatureStream) {
			fail(c, http.StatusPaymentRequired, "LICENSE_FEATURE_DENIED", "当前授权不包含四层代理（stream）")
			return
		}
	}

	existingSites := map[string]*store.Site{}
	if len(srcSites) > 0 {
		dstSites, err := q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(targetID)).Find()
		if err != nil {
			failErr(c, err)
			return
		}
		for _, ds := range dstSites {
			existingSites[ds.Name] = ds
		}
		toCreate := 0
		for _, ss := range srcSites {
			if _, ok := existingSites[ss.Name]; !ok {
				toCreate++
			}
		}
		snap := s.Lic.Snapshot()
		if snap.Payload != nil && snap.Payload.Limits.Sites > 0 && toCreate > 0 {
			total, err := q.Site.WithContext(ctx).Count()
			if err != nil {
				failErr(c, err)
				return
			}
			if total+int64(toCreate) > int64(snap.Payload.Limits.Sites) {
				fail(c, http.StatusPaymentRequired, "LICENSE_SITE_LIMIT", fmt.Sprintf("复制后将超出授权站点数上限（%d）", snap.Payload.Limits.Sites))
				return
			}
		}
	}

	var upStat, ipStat, siteStat, streamStat copyCount
	targetAllowed := parseAllowedPrefixes(targetGroup.AllowedCIDRs)

	// 3–6. 事务内统一写入目标组草稿，避免中途出错留下半成品
	err = s.Store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tq := query.Use(tx)

		// 3. 复制上游
		if req.IncludeUpstreams || len(needUpstreams) > 0 {
			srcUps, err := tq.Upstream.WithContext(ctx).Where(tq.Upstream.GroupID.Eq(req.SourceGroupID)).Order(tq.Upstream.Name).Find()
			if err != nil {
				return err
			}
			dstUps, err := tq.Upstream.WithContext(ctx).Where(tq.Upstream.GroupID.Eq(targetID)).Find()
			if err != nil {
				return err
			}
			existingUps := map[string]*store.Upstream{}
			for _, u := range dstUps {
				existingUps[u.Name] = u
			}
			for _, su := range srcUps {
				if !req.IncludeUpstreams && !needUpstreams[su.Name] {
					continue
				}
				var ug render.UpstreamGroup
				if json.Unmarshal([]byte(su.Spec), &ug) == nil {
					for _, srv := range ug.Servers {
						if err := render.ValidateUpstream(srv.Addr, targetAllowed); err != nil {
							warnings = append(warnings, fmt.Sprintf("上游 %s 的后端 %s 不在目标组允许回源网段内，请按目标机房修改后再发布", su.Name, srv.Addr))
							break
						}
					}
				}
				if cur, exists := existingUps[su.Name]; exists {
					if !req.Overwrite {
						upStat.Skipped++
						continue
					}
					cur.Spec = su.Spec
					cur.UpdatedBy = user
					if err := tq.Upstream.WithContext(ctx).Save(cur); err != nil {
						return err
					}
					upStat.Updated++
				} else {
					nw := &store.Upstream{GroupID: targetID, Name: su.Name, Spec: su.Spec, UpdatedBy: user}
					if err := tq.Upstream.WithContext(ctx).Create(nw); err != nil {
						return err
					}
					upStat.Created++
				}
			}
		}

		// 4. 复制组内私有 IP 集（全局 IP 集已跨组共享，无需复制）
		if req.IncludeIPSets || len(needIPSets) > 0 {
			srcIPs, err := tq.IPSet.WithContext(ctx).Where(tq.IPSet.GroupID.Eq(req.SourceGroupID)).Order(tq.IPSet.Name).Find()
			if err != nil {
				return err
			}
			dstIPs, err := tq.IPSet.WithContext(ctx).Where(tq.IPSet.GroupID.Eq(targetID)).Find()
			if err != nil {
				return err
			}
			existingIPs := map[string]*store.IPSet{}
			for _, ip := range dstIPs {
				existingIPs[ip.Name] = ip
			}
			for _, sip := range srcIPs {
				if !req.IncludeIPSets && !needIPSets[sip.Name] {
					continue
				}
				if cur, exists := existingIPs[sip.Name]; exists {
					if !req.Overwrite {
						ipStat.Skipped++
						continue
					}
					cur.Label, cur.Source, cur.CIDRs, cur.Count, cur.UpdatedBy = sip.Label, sip.Source, sip.CIDRs, sip.Count, user
					if err := tq.IPSet.WithContext(ctx).Save(cur); err != nil {
						return err
					}
					ipStat.Updated++
				} else {
					if len(existingIPs)+ipStat.Created >= render.MaxIPSets {
						warnings = append(warnings, fmt.Sprintf("目标组 IP 集数量已达上限（%d），跳过 %s", render.MaxIPSets, sip.Name))
						ipStat.Skipped++
						continue
					}
					nw := &store.IPSet{GroupID: targetID, Name: sip.Name, Label: sip.Label, Source: sip.Source, CIDRs: sip.CIDRs, Count: sip.Count, UpdatedBy: user}
					if err := tq.IPSet.WithContext(ctx).Create(nw); err != nil {
						return err
					}
					ipStat.Created++
				}
			}
		}

		// 5. 复制站点（若路由绑定了静态资源包，按 SHA-256 复用或复制到目标组；检测目标组内域名冲突）
		if len(srcSites) > 0 {
			domainOwner := map[string]string{}
			for _, ds := range existingSites {
				var rs render.Site
				if json.Unmarshal([]byte(ds.Spec), &rs) == nil {
					for _, dm := range rs.Domains {
						domainOwner[strings.ToLower(strings.TrimSpace(dm))] = ds.Name
					}
				}
			}
			bundleMap := map[uint]uint{}
			cloneBundle := func(srcBundleID uint) (uint, error) {
				if dstID, ok := bundleMap[srcBundleID]; ok {
					return dstID, nil
				}
				sb, err := tq.StaticBundle.WithContext(ctx).Where(tq.StaticBundle.ID.Eq(srcBundleID), tq.StaticBundle.GroupID.Eq(req.SourceGroupID)).First()
				if err != nil {
					return 0, err
				}
				if same, err := tq.StaticBundle.WithContext(ctx).Where(tq.StaticBundle.GroupID.Eq(targetID), tq.StaticBundle.SHA256.Eq(sb.SHA256)).First(); err == nil {
					bundleMap[srcBundleID] = same.ID
					return same.ID, nil
				}
				cp := &store.StaticBundle{GroupID: targetID, Name: sb.Name, SHA256: sb.SHA256, Size: sb.Size, Files: sb.Files, Data: sb.Data, UploadedBy: user}
				if err := tq.StaticBundle.WithContext(ctx).Create(cp); err != nil {
					return 0, err
				}
				bundleMap[srcBundleID] = cp.ID
				return cp.ID, nil
			}

			for _, ss := range srcSites {
				cur, exists := existingSites[ss.Name]
				if exists && !req.Overwrite {
					siteStat.Skipped++
					continue
				}
				specStr := ss.Spec
				var rs render.Site
				if json.Unmarshal([]byte(ss.Spec), &rs) == nil {
					for _, dm := range rs.Domains {
						k := strings.ToLower(strings.TrimSpace(dm))
						if owner, taken := domainOwner[k]; taken && owner != ss.Name {
							warnings = append(warnings, fmt.Sprintf("站点 %s 的域名 %s 与目标组站点 %s 冲突，请调整后再发布", ss.Name, dm, owner))
						}
						domainOwner[k] = ss.Name
					}
					changed := false
					for i := range rs.Routes {
						if rs.Routes[i].Mode == "static" && rs.Routes[i].Static != nil && rs.Routes[i].Static.BundleID != 0 {
							newID, berr := cloneBundle(rs.Routes[i].Static.BundleID)
							if berr != nil {
								rs.Routes[i].Static.BundleID = 0
								warnings = append(warnings, fmt.Sprintf("站点 %s 的静态资源包未找到，已重置为内联页面", ss.Name))
							} else {
								rs.Routes[i].Static.BundleID = newID
							}
							changed = true
						}
					}
					if changed {
						if b, merr := json.Marshal(rs); merr == nil {
							specStr = string(b)
						}
					}
				}
				if exists {
					cur.Spec = specStr
					cur.Enabled = ss.Enabled
					cur.Version++
					cur.UpdatedBy = user
					if err := tq.Site.WithContext(ctx).Save(cur); err != nil {
						return err
					}
					siteStat.Updated++
				} else {
					nw := &store.Site{GroupID: targetID, Name: ss.Name, Spec: specStr, Enabled: ss.Enabled, Version: 1, UpdatedBy: user}
					if err := tq.Site.WithContext(ctx).Create(nw); err != nil {
						return err
					}
					siteStat.Created++
				}
			}
		}

		// 6. 复制四层代理（同步提醒回源网段与端口冲突）
		if len(srcStreams) > 0 {
			dstStreams, err := tq.StreamService.WithContext(ctx).Where(tq.StreamService.GroupID.Eq(targetID)).Find()
			if err != nil {
				return err
			}
			existingStreams := map[string]*store.StreamService{}
			listenOwner := map[string]string{}
			for _, ds := range dstStreams {
				existingStreams[ds.Name] = ds
				var sp render.StreamProxy
				if json.Unmarshal([]byte(ds.Spec), &sp) == nil && sp.Listen > 0 {
					listenOwner[fmt.Sprintf("%s/%d", sp.Protocol, sp.Listen)] = ds.Name
				}
			}
			for _, st := range srcStreams {
				var sp render.StreamProxy
				if json.Unmarshal([]byte(st.Spec), &sp) == nil {
					if sp.Listen > 0 {
						lk := fmt.Sprintf("%s/%d", sp.Protocol, sp.Listen)
						if owner, taken := listenOwner[lk]; taken && owner != st.Name {
							warnings = append(warnings, fmt.Sprintf("四层代理 %s 的监听端口 %s 与目标组 %s 冲突，请调整后再发布", st.Name, lk, owner))
						}
						listenOwner[lk] = st.Name
					}
					for _, srv := range sp.Servers {
						if err := render.ValidateUpstream(srv.Addr, targetAllowed); err != nil {
							warnings = append(warnings, fmt.Sprintf("四层代理 %s 的后端 %s 不在目标组允许回源网段内，请按目标机房修改后再发布", st.Name, srv.Addr))
							break
						}
					}
				}
				if cur, exists := existingStreams[st.Name]; exists {
					if !req.Overwrite {
						streamStat.Skipped++
						continue
					}
					cur.Spec, cur.Enabled, cur.UpdatedBy = st.Spec, st.Enabled, user
					if err := tq.StreamService.WithContext(ctx).Save(cur); err != nil {
						return err
					}
					streamStat.Updated++
				} else {
					nw := &store.StreamService{GroupID: targetID, Name: st.Name, Spec: st.Spec, Enabled: st.Enabled, UpdatedBy: user}
					if err := tq.StreamService.WithContext(ctx).Create(nw); err != nil {
						return err
					}
					streamStat.Created++
				}
			}
		}
		return nil
	})
	if err != nil {
		failErr(c, err)
		return
	}

	s.audit(c, "group.copy_config", "node_group", fmt.Sprint(targetID), map[string]any{
		"source_group": sourceGroup.Name, "target_group": targetGroup.Name, "overwrite": req.Overwrite,
		"upstreams": upStat, "ip_sets": ipStat, "sites": siteStat, "streams": streamStat,
	})
	c.JSON(http.StatusOK, gin.H{
		"source_group": sourceGroup.Name,
		"target_group": targetGroup.Name,
		"upstreams":    upStat,
		"ip_sets":      ipStat,
		"sites":        siteStat,
		"streams":      streamStat,
		"warnings":     warnings,
	})
}
