package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/importer"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 配置导入（规格书 12.4）：上传 → 生成报告（落库，可下载）→ 确认后应用为草稿。
// 证书与私钥只在内存中保留 30 分钟等待确认，不写入报告、日志或响应；应用时进入证书库（加密存储）。

const importCertTTL = 30 * time.Minute

type pendingCerts struct {
	certs   map[string]importer.CertMaterial
	expires time.Time
}

var importCerts sync.Map // import id -> *pendingCerts

func takeCerts(id uint) map[string]importer.CertMaterial {
	v, ok := importCerts.LoadAndDelete(id)
	if !ok {
		return nil
	}
	p := v.(*pendingCerts)
	if time.Now().After(p.expires) {
		return nil
	}
	return p.certs
}

func sweepCerts() {
	importCerts.Range(func(k, v any) bool {
		if time.Now().After(v.(*pendingCerts).expires) {
			importCerts.Delete(k)
		}
		return true
	})
}

const maxImportUpload = 20 << 20

// createImport：multipart 字段 group_id、file（-T 输出的文本或配置目录 tar.gz）。
func (s *Server) createImport(c *gin.Context) {
	sweepCerts()
	gid := queryGroup(c)
	if gid == 0 {
		fmt.Sscan(c.PostForm("group_id"), &gid)
	}
	g, err := store.First[store.NodeGroup](s.Store, gid)
	if err != nil {
		failErr(c, err)
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		fail(c, http.StatusBadRequest, "NO_FILE", "请选择要导入的文件")
		return
	}
	if fh.Size > maxImportUpload {
		fail(c, http.StatusRequestEntityTooLarge, "TOO_LARGE", "文件超过 20 MiB")
		return
	}
	f, err := fh.Open()
	if err != nil {
		failErr(c, err)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImportUpload+1))
	if err != nil {
		failErr(c, err)
		return
	}
	var allowed []netip.Prefix
	for _, p := range strings.Split(g.AllowedCIDRs, ",") {
		if pp, err := netip.ParsePrefix(strings.TrimSpace(p)); err == nil {
			allowed = append(allowed, pp)
		}
	}
	opts := importer.Options{AllowedNet: allowed}
	var rep *importer.Report
	source := "dump"
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		source = "archive"
		rep, err = importer.FromArchive(data, opts)
	} else {
		rep, err = importer.FromDump(data, opts)
	}
	if err != nil {
		fail(c, http.StatusBadRequest, "IMPORT_PARSE_FAILED", err.Error())
		return
	}
	rj, _ := json.Marshal(rep)
	sum := sha256.Sum256(rj)
	row := &store.ConfigImport{GroupID: gid, Source: source, FileName: fh.Filename, Report: string(rj), ReportSHA256: hex.EncodeToString(sum[:]),
		Sites: rep.Summary.Sites, Full: rep.Summary.Full, Partial: rep.Summary.Partial, Failed: rep.Summary.Failed, Issues: rep.Summary.Issues,
		CreatedBy: c.GetString("user")}
	if err := s.q().ConfigImport.WithContext(c.Request.Context()).Create(row); err != nil {
		failErr(c, err)
		return
	}
	if len(rep.Certificates) > 0 {
		importCerts.Store(row.ID, &pendingCerts{certs: rep.Certificates, expires: time.Now().Add(importCertTTL)})
	}
	s.audit(c, "config.import_analyze", "config_import", fmt.Sprint(row.ID), map[string]any{"group": gid, "file": fh.Filename, "source": source,
		"report_sha256": row.ReportSHA256, "summary": rep.Summary})
	c.JSON(http.StatusOK, gin.H{"import": row, "report": rep})
}

func (s *Server) listImports(c *gin.Context) {
	ci := s.q().ConfigImport
	sel := ci.WithContext(c.Request.Context()).Order(ci.ID.Desc()).Limit(50)
	if g := queryGroup(c); g != 0 {
		sel = sel.Where(ci.GroupID.Eq(g))
	}
	rows, err := sel.Find()
	if err != nil {
		failErr(c, err)
		return
	}
	if rows == nil {
		rows = []*store.ConfigImport{}
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) loadImport(c *gin.Context) (*store.ConfigImport, *importer.Report, bool) {
	id, ok := parseID(c)
	if !ok {
		return nil, nil, false
	}
	ci := s.q().ConfigImport
	row, err := ci.WithContext(c.Request.Context()).Where(ci.ID.Eq(id)).First()
	if err != nil {
		failErr(c, store.ErrNotFound)
		return nil, nil, false
	}
	var rep importer.Report
	if err := json.Unmarshal([]byte(row.Report), &rep); err != nil {
		failErr(c, err)
		return nil, nil, false
	}
	return row, &rep, true
}

func (s *Server) getImport(c *gin.Context) {
	row, rep, ok := s.loadImport(c)
	if !ok {
		return
	}
	var result any
	if row.Result != "" {
		_ = json.Unmarshal([]byte(row.Result), &result)
	}
	_, certsPending := importCerts.Load(row.ID)
	c.JSON(http.StatusOK, gin.H{"import": row, "report": rep, "result": result, "certs_pending": certsPending})
}

// downloadImport 下载报告（JSON），留存审计。
func (s *Server) downloadImport(c *gin.Context) {
	row, _, ok := s.loadImport(c)
	if !ok {
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="import-report-%d.json"`, row.ID))
	c.Data(http.StatusOK, "application/json", []byte(row.Report))
}

// ApplyResult 是应用导入的结果。
type ApplyResult struct {
	Upstreams []string `json:"upstreams"`
	Certs     []string `json:"certificates"`
	Sites     []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Note    string `json:"note,omitempty"`
	} `json:"sites"`
	Snippets []uint   `json:"snippets"` // 已提交、待另一人审批的片段
	Errors   []string `json:"errors"`
}

// applyImport 把报告中的结果写成草稿：上游、证书、站点（有需人工处理的问题时停用）、片段（待审批）。
// 只生成草稿，之后照常预览 → 审批 → 发布。
func (s *Server) applyImport(c *gin.Context) {
	row, rep, ok := s.loadImport(c)
	if !ok {
		return
	}
	user, ip := c.GetString("user"), c.ClientIP()
	ci := s.q().ConfigImport
	now := time.Now()
	res0, err := ci.WithContext(c.Request.Context()).Where(ci.ID.Eq(row.ID), ci.AppliedAt.IsNull()).UpdateSimple(ci.AppliedBy.Value(user), ci.AppliedAt.Value(now))
	if err != nil {
		failErr(c, err)
		return
	}
	if res0.RowsAffected != 1 {
		fail(c, http.StatusConflict, "ALREADY_APPLIED", "该导入已应用过")
		return
	}
	certMat := takeCerts(row.ID)
	ctx := c.Request.Context()
	q := s.q()
	res := ApplyResult{Upstreams: []string{}, Certs: []string{}, Snippets: []uint{}, Errors: []string{}}

	// 上游：同名且内容相同则沿用，不同则改名
	upName := map[string]string{}
	for _, u := range rep.Upstreams {
		name := u.Spec.Name
		for i := 2; ; i++ {
			ex, err := q.Upstream.WithContext(ctx).Where(q.Upstream.GroupID.Eq(row.GroupID), q.Upstream.Name.Eq(name)).First()
			if err != nil {
				break
			}
			want, _ := json.Marshal(render.UpstreamGroup{Name: name, Algorithm: u.Spec.Algorithm, Servers: u.Spec.Servers})
			if ex.Spec == string(want) {
				break
			}
			name = fmt.Sprintf("%s-%d", u.Spec.Name, i)
		}
		upName[u.Spec.Name] = name
		spec := u.Spec
		spec.Name = name
		sj, _ := json.Marshal(spec)
		if _, err := q.Upstream.WithContext(ctx).Where(q.Upstream.GroupID.Eq(row.GroupID), q.Upstream.Name.Eq(name)).First(); err == nil {
			continue // 已存在且相同
		}
		if err := s.validateWith(row.GroupID, nil, &spec, "", ""); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("上游 %s：%v", name, err))
			continue
		}
		if err := q.Upstream.WithContext(ctx).Create(&store.Upstream{GroupID: row.GroupID, Name: name, Spec: string(sj), UpdatedBy: user}); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("上游 %s：%v", name, err))
			continue
		}
		res.Upstreams = append(res.Upstreams, name)
	}

	snap := s.Lic.Snapshot()
	for _, sr := range rep.Sites {
		if sr.Status == importer.StatusFailed {
			continue
		}
		name := sr.Name
		for i := 2; ; i++ {
			if _, err := q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(row.GroupID), q.Site.Name.Eq(name)).First(); err != nil {
				break
			}
			name = fmt.Sprintf("%s-%d", sr.Name, i)
		}
		if snap.Payload != nil && snap.Payload.Limits.Sites > 0 {
			if n, _ := q.Site.WithContext(ctx).Count(); n >= int64(snap.Payload.Limits.Sites) {
				res.Errors = append(res.Errors, fmt.Sprintf("站点 %s：已达到授权站点数上限（%d），未导入", sr.Name, snap.Payload.Limits.Sites))
				continue
			}
		}
		spec := sr.Spec
		spec.Name = name
		for i := range spec.Routes {
			if spec.Routes[i].Proxy != nil {
				if n, ok := upName[spec.Routes[i].Proxy.Upstream]; ok {
					spec.Routes[i].Proxy = &render.ProxySpec{Upstream: n}
				}
			}
		}
		enabled := !sr.Blocking()
		note := ""
		if spec.HTTPS != nil {
			spec.HTTPS = &render.TLS{}
			if m, ok := certMat[sr.CertName]; ok && sr.CertFound {
				cert, err := s.Certs.CreateFrom("import", uniqueCertName(s, name), m.CertPEM, m.KeyPEM, user)
				if err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("站点 %s 的证书：%v", name, err))
					enabled, note = false, "证书导入失败，请在证书库上传后关联"
				} else {
					spec.HTTPS.CertID = cert.ID
					res.Certs = append(res.Certs, cert.Name)
				}
			} else {
				enabled = false
				note = "证书需在证书库上传后关联"
				if sr.CertFound {
					note = "证书已过期未确认（30 分钟），请重新上传或在证书库上传后关联"
				}
			}
		}
		if enabled {
			if err := s.validateWith(row.GroupID, &spec, nil, "", ""); err != nil {
				enabled, note = false, "校验未通过："+err.Error()
			}
		} else if note == "" {
			note = "有需人工处理的指令，处理后启用"
		}
		sj, _ := json.Marshal(spec)
		st := &store.Site{GroupID: row.GroupID, Name: name, Spec: string(sj), Enabled: enabled, Version: 1, UpdatedBy: user}
		if err := q.Site.WithContext(ctx).Create(st); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("站点 %s：%v", name, err))
			continue
		}
		res.Sites = append(res.Sites, struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Note    string `json:"note,omitempty"`
		}{name, enabled, note})
		s.audit(c, "site.create", "site", name, map[string]any{"group_id": row.GroupID, "domains": spec.Domains, "origin": "import", "import": row.ID, "enabled": enabled})

		submit := func(in deploy.SnippetInput) {
			in.GroupID, in.SiteID, in.Origin, in.Note = row.GroupID, st.ID, "import", fmt.Sprintf("配置导入 #%d", row.ID)
			sn, issues, err := s.Deploy.SubmitSnippet(in, user, ip)
			switch {
			case err != nil:
				res.Errors = append(res.Errors, fmt.Sprintf("站点 %s 的片段：%v", name, err))
			case len(issues) > 0:
				res.Errors = append(res.Errors, fmt.Sprintf("站点 %s 的片段：%s", name, issues[0]))
			default:
				res.Snippets = append(res.Snippets, sn.ID)
			}
		}
		if sr.ServerSnippet != "" {
			submit(deploy.SnippetInput{Scope: "server", Content: sr.ServerSnippet})
		}
		for p, txt := range sr.LocationSnippets {
			submit(deploy.SnippetInput{Scope: "location", RoutePath: p, Content: txt})
		}
	}
	if rep.HTTPSnippet != "" {
		sn, issues, err := s.Deploy.SubmitSnippet(deploy.SnippetInput{GroupID: row.GroupID, Scope: "http", Name: fmt.Sprintf("import-%d", row.ID),
			Content: rep.HTTPSnippet, Origin: "import", Note: fmt.Sprintf("配置导入 #%d", row.ID)}, user, ip)
		if err == nil && len(issues) == 0 {
			res.Snippets = append(res.Snippets, sn.ID)
		} else if err != nil {
			res.Errors = append(res.Errors, "http 片段："+err.Error())
		}
	}
	rj, _ := json.Marshal(res)
	_, _ = ci.WithContext(ctx).Where(ci.ID.Eq(row.ID)).Update(ci.Result, string(rj))
	s.audit(c, "config.import_apply", "config_import", fmt.Sprint(row.ID), map[string]any{"group": row.GroupID, "report_sha256": row.ReportSHA256,
		"upstreams": res.Upstreams, "certificates": res.Certs, "sites": len(res.Sites), "snippets": res.Snippets, "errors": len(res.Errors)})
	c.JSON(http.StatusOK, res)
}

var bgCtx = context.Background()

func uniqueCertName(s *Server, base string) string {
	name := "imp-" + base
	for i := 2; ; i++ {
		cq := s.q().Certificate
		if _, err := cq.WithContext(bgCtx).Where(cq.Name.Eq(name)).First(); err != nil {
			return name
		}
		name = fmt.Sprintf("imp-%s-%d", base, i)
	}
}
