package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/importer"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// HandleAgentConfigSnapshot 处理独立节点注册/运行后通过 mTLS 主动上报的本机配置快照（tar.gz）。
// 始终先解析并保存导入报告；若节点组尚无站点/上游草稿、处于安全模式（观察接入或尚未发布）且不含原生 Lua，
// 自动将可转换的上游/站点/证书/白名单片段生成为草稿，供运维直接核对后显式发布（绝不自动覆盖现网节点）。
func (s *Server) HandleAgentConfigSnapshot(ctx context.Context, node *store.Node, ip string, archive []byte) (*agentv1.ConfigSnapshotResponse, error) {
	sweepCerts()
	g, err := store.First[store.NodeGroup](s.Store, node.GroupID)
	if err != nil {
		return nil, err
	}
	var allowed []netip.Prefix
	for _, p := range strings.Split(g.AllowedCIDRs, ",") {
		if pp, err := netip.ParsePrefix(strings.TrimSpace(p)); err == nil {
			allowed = append(allowed, pp)
		}
	}
	opts := importer.Options{AllowedNet: allowed}
	if s.Deploy != nil {
		if prof, perr := s.Deploy.Profile(node.GroupID); perr == nil {
			opts.HTTPPort = prof.HTTP()
			opts.HTTPSPort = prof.HTTPS()
		}
	}
	rep, err := importer.FromArchive(archive, opts)
	if err != nil {
		return nil, fmt.Errorf("解析上报配置失败：%w", err)
	}
	rj, err := json.Marshal(rep)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(rj)
	actor := "agent:" + node.Name
	fileName := fmt.Sprintf("node:%s (自动上报)", node.Name)
	row := &store.ConfigImport{
		GroupID:      node.GroupID,
		Source:       "agent",
		FileName:     fileName,
		Report:       string(rj),
		ReportSHA256: hex.EncodeToString(sum[:]),
		Sites:        rep.Summary.Sites,
		Full:         rep.Summary.Full,
		Partial:      rep.Summary.Partial,
		Failed:       rep.Summary.Failed,
		Issues:       rep.Summary.Issues,
		CreatedBy:    actor,
	}
	if err := s.q().ConfigImport.WithContext(ctx).Create(row); err != nil {
		return nil, err
	}
	if len(rep.Certificates) > 0 {
		importCerts.Store(row.ID, &pendingCerts{certs: rep.Certificates, expires: time.Now().Add(importCertTTL)})
	}
	if s.Audit != nil {
		_ = s.Audit.Write(auditE{
			Username: actor, IP: ip, Action: "config.import_analyze", ObjectType: "config_import", ObjectID: fmt.Sprint(row.ID),
			Detail: map[string]any{"group": node.GroupID, "node_id": node.ID, "node": node.Name, "source": "agent",
				"report_sha256": row.ReportSHA256, "summary": rep.Summary},
		})
	}

	requiresLegacy := rep.Lua.RequiresLegacy
	resp := &agentv1.ConfigSnapshotResponse{
		ImportId:       uint64(row.ID),
		Sites:          int32(rep.Summary.Sites),
		Full:           int32(rep.Summary.Full),
		Partial:        int32(rep.Summary.Partial),
		Failed:         int32(rep.Summary.Failed),
		Issues:         int32(rep.Summary.Issues),
		RequiresLegacy: requiresLegacy,
	}

	// 串行化“检查空组 + 自动落草稿”，避免两台独立节点并发注册时在同组生成重复站点草稿
	s.agentImportMu.Lock()
	defer s.agentImportMu.Unlock()

	q := s.q()
	siteCount, _ := q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(node.GroupID)).Count()
	upCount, _ := q.Upstream.WithContext(ctx).Where(q.Upstream.GroupID.Eq(node.GroupID)).Count()
	mode := deploy.ManagementModeManaged
	if s.Deploy != nil {
		if m, merr := s.Deploy.ManagementMode(node.GroupID); merr == nil {
			mode = m
		}
	}
	safeAuto := (mode == deploy.ManagementModeObserve || (mode == deploy.ManagementModeManaged && g.CurrentRevisionID == "")) &&
		siteCount == 0 && upCount == 0 && !requiresLegacy && (rep.Summary.Full+rep.Summary.Partial > 0)

	if safeAuto {
		res, statusCode, _, errMsg := s.applyImportByID(ctx, row.ID, actor, ip)
		if statusCode == 0 && res != nil {
			resp.AutoApplied = true
			resp.AppliedSites = int32(len(res.Sites))
			resp.AppliedUpstreams = int32(len(res.Upstreams))
			resp.AppliedCerts = int32(len(res.Certs))
			resp.AppliedSnippets = int32(len(res.Snippets))
			resp.Message = fmt.Sprintf("已自动导入为控制台草稿：站点 %d、上游 %d、证书 %d、待审片段 %d；请在控制台核对后显式发布",
				len(res.Sites), len(res.Upstreams), len(res.Certs), len(res.Snippets))
			return resp, nil
		}
		resp.Message = fmt.Sprintf("已生成导入报告 #%d（自动转草稿未完成：%s），请在控制台“配置导入”页查看并应用", row.ID, errMsg)
		return resp, nil
	}

	switch {
	case requiresLegacy:
		resp.Message = fmt.Sprintf("已生成导入报告 #%d：检测到原生 Lua 配置，建议保留为兼容配置（legacy）或在控制台“配置导入”页核对", row.ID)
	case siteCount > 0 || upCount > 0:
		resp.Message = fmt.Sprintf("已生成导入报告 #%d（目标节点组已有草稿配置，未自动覆盖），请在控制台“配置导入”页核对后按需应用", row.ID)
	case rep.Summary.Sites == 0:
		resp.Message = fmt.Sprintf("已生成导入报告 #%d：未检测到可导入的业务站点（当前可能是默认基线配置）", row.ID)
	default:
		resp.Message = fmt.Sprintf("已生成导入报告 #%d，请在控制台“配置导入”页核对后应用为草稿", row.ID)
	}
	return resp, nil
}
