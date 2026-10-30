package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/textdiff"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// 漂移的查看与采纳（规格书 12.7 路径 B）。采纳 = 把节点上的手工改动转成自定义片段（白名单外的不能采纳），
// 作者记为“节点手工修改 + 采纳人”，同样需要另一人批准并发布；发布后节点回到受控版本，漂移消除。

// DriftFileDiff 是漂移文件与节点应有内容的差异。
type DriftFileDiff struct {
	gateway.DriftFileView
	Diff string `json:"diff"`
}

// DriftDetail 是一台节点的漂移详情。
type DriftDetail struct {
	NodeID     uint            `json:"node_id"`
	NodeName   string          `json:"node_name"`
	RevisionID string          `json:"revision_id"`
	ReportedAt string          `json:"reported_at"`
	Files      []DriftFileDiff `json:"files"`
}

var ErrNoDriftReport = errors.New("该节点没有漂移差异上报（可能 agent 版本较旧，或已恢复一致）")

func (d *Service) loadDrift(nodeID uint) (*store.Node, *store.NodeDrift, []gateway.DriftFileView, map[string][]byte, error) {
	n, err := store.First[store.Node](d.Store, nodeID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	q := d.q()
	row, err := q.NodeDrift.WithContext(bg).Where(q.NodeDrift.NodeID.Eq(nodeID)).First()
	if err != nil {
		return nil, nil, nil, nil, ErrNoDriftReport
	}
	var files []gateway.DriftFileView
	if err := json.Unmarshal([]byte(row.Files), &files); err != nil {
		return nil, nil, nil, nil, err
	}
	rev, err := d.revision(row.RevisionID, n.GroupID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("revision %s not found", row.RevisionID)
	}
	want, err := render.ReadTarGz(rev.Bundle)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return n, row, files, want, nil
}

// DriftView 返回漂移文件相对应有内容的差异（私钥与超限文件只显示哈希）。
func (d *Service) DriftView(nodeID uint) (*DriftDetail, error) {
	n, row, files, want, err := d.loadDrift(nodeID)
	if err != nil {
		return nil, err
	}
	out := &DriftDetail{NodeID: n.ID, NodeName: n.Name, RevisionID: row.RevisionID, ReportedAt: row.ReportedAt.Format("2006-01-02 15:04:05"), Files: []DriftFileDiff{}}
	for _, f := range files {
		x := DriftFileDiff{DriftFileView: f}
		switch {
		case strings.HasSuffix(f.Path, ".key"):
			x.Diff = "（私钥文件只比较哈希：" + f.SHA256 + "）"
		case f.Omitted:
			x.Diff = "（文件超过上报上限，只比较哈希：" + f.SHA256 + "）"
		case f.Change == "removed":
			x.Diff = textdiff.Unified(f.Path, string(want[f.Path]), "")
		default:
			x.Diff = textdiff.Unified(f.Path, string(want[f.Path]), f.Content)
		}
		x.Content = "" // 详情只给差异
		out.Files = append(out.Files, x)
	}
	return out, nil
}

// AdoptResult 是采纳的结果。
type AdoptResult struct {
	Snippets []uint   `json:"snippets"` // 已提交、待另一人批准的片段
	Rejected []string `json:"rejected"` // 不能采纳的改动（只能恢复）
}

var siteFileRe = regexp.MustCompile(`^conf\.d/site-([a-z0-9][a-z0-9-]{1,62})\.conf$`)

func key(d *conf.Directive) string {
	cp := *d
	if d.Name == "location" {
		cp.Block = []*conf.Directive{}
	}
	return strings.TrimSpace(conf.Print([]*conf.Directive{&cp}, 0))
}

// diffBlock 返回 actual 中新增的指令；expected 中被删除或修改的指令记入 removed。
func diffBlock(expected, actual []*conf.Directive) (added []*conf.Directive, removed []string) {
	have := map[string]int{}
	for _, e := range expected {
		if e.Name != "location" {
			have[key(e)]++
		}
	}
	for _, a := range actual {
		if a.Name == "location" {
			continue
		}
		k := key(a)
		if have[k] > 0 {
			have[k]--
			continue
		}
		added = append(added, a)
	}
	for k, n := range have {
		if n > 0 {
			removed = append(removed, k)
		}
	}
	return
}

// AdoptDrift 把节点上的手工改动转成待审批的片段。
func (d *Service) AdoptDrift(nodeID uint, user, ip string) (*AdoptResult, error) {
	n, row, files, want, err := d.loadDrift(nodeID)
	if err != nil {
		return nil, err
	}
	res := &AdoptResult{Snippets: []uint{}, Rejected: []string{}}
	q := d.q()
	note := fmt.Sprintf("节点 %s 手工修改，由 %s 采纳（%s）", n.Name, user, row.RevisionID)
	reject := func(path, why string) { res.Rejected = append(res.Rejected, path+"："+why) }
	for _, f := range files {
		m := siteFileRe.FindStringSubmatch(f.Path)
		if m == nil || f.Change != "modified" || f.Omitted {
			reject(f.Path, "只能采纳站点配置文件中的改动，其余只能恢复")
			continue
		}
		st, err := q.Site.WithContext(bg).Where(q.Site.GroupID.Eq(n.GroupID), q.Site.Name.Eq(m[1])).First()
		if err != nil {
			reject(f.Path, "站点已不存在")
			continue
		}
		exp, err1 := conf.Parse(f.Path, want[f.Path])
		act, err2 := conf.Parse(f.Path, []byte(f.Content))
		if err1 != nil || err2 != nil {
			reject(f.Path, "节点上的文件无法解析")
			continue
		}
		if len(exp) != len(act) || len(act) == 0 {
			reject(f.Path, "新增或删除了 server 块")
			continue
		}
		// 只有最后一个 server（站点主 server）可以采纳；跳转 server 中的改动只能恢复
		bad := false
		for i := 0; i < len(act)-1; i++ {
			if a, r := diffBlock(exp[i].Block, act[i].Block); len(a)+len(r) > 0 {
				reject(f.Path, "HTTP 跳转 server 中的改动不能采纳")
				bad = true
			}
		}
		if bad {
			continue
		}
		es, as := exp[len(exp)-1], act[len(act)-1]
		added, removed := diffBlock(es.Block, as.Block)
		if len(removed) > 0 {
			reject(f.Path, "删除或修改了渲染器生成的指令（"+strings.Join(removed, "；")+"）")
			continue
		}
		expLoc := map[string]*conf.Directive{}
		for _, x := range es.Block {
			if x.Name == "location" {
				expLoc[strings.Join(x.Args, " ")] = x
			}
		}
		locAdds := map[string][]*conf.Directive{}
		for _, x := range as.Block {
			if x.Name != "location" {
				continue
			}
			k := strings.Join(x.Args, " ")
			e, ok := expLoc[k]
			if !ok {
				added = append(added, x) // 新增的 location 整块进 server 片段
				continue
			}
			a, r := diffBlock(e.Block, x.Block)
			if len(r) > 0 {
				reject(f.Path, "location "+k+" 中删除或修改了渲染器生成的指令")
				bad = true
				break
			}
			if len(a) > 0 {
				path := x.Args[len(x.Args)-1]
				locAdds[path] = append(locAdds[path], a...)
			}
		}
		if bad {
			continue
		}
		submit := func(scope, route string, extra []*conf.Directive) {
			if len(extra) == 0 {
				return
			}
			sp := q.Snippet
			cur, _ := sp.WithContext(bg).Where(sp.GroupID.Eq(n.GroupID), sp.SiteID.Eq(st.ID), sp.Scope.Eq(scope), sp.RoutePath.Eq(route), sp.Name.Eq("")).First()
			text := conf.Print(extra, 0)
			if cur != nil && cur.Content != "" {
				text = cur.Content + text
			}
			sn, issues, err := d.SubmitSnippet(SnippetInput{GroupID: n.GroupID, SiteID: st.ID, Scope: scope, RoutePath: route, Content: text, Note: note, Origin: "drift"}, user, ip)
			switch {
			case err != nil:
				reject(f.Path, err.Error())
			case len(issues) > 0:
				for _, is := range issues {
					reject(f.Path, fmt.Sprintf("%s 不在白名单内（%s），只能恢复", is.Directive, is.Message))
				}
			default:
				res.Snippets = append(res.Snippets, sn.ID)
			}
		}
		submit("server", "", added)
		for p, xs := range locAdds {
			submit("location", p, xs)
		}
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "node.drift_adopt", ObjectType: "node", ObjectID: n.Name,
		Detail: map[string]any{"revision": row.RevisionID, "tree_hash": row.TreeHash, "snippets": res.Snippets, "rejected": res.Rejected}})
	return res, nil
}
