package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/textdiff"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
	"gorm.io/gen/field"
)

// 自定义片段（规格书 12.7 路径 A）：任何新增、修改、删除都要另一人批准（不论是否购买“发布审批”），
// 批准后的内容才参与渲染；之后仍要正常发布（或走发布审批）才会下发到节点。

// SnippetInput 是一次片段提交。
type SnippetInput struct {
	GroupID   uint   `json:"group_id"`
	SiteID    uint   `json:"site_id"`
	Scope     string `json:"scope"`
	RoutePath string `json:"route_path"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Note      string `json:"note"`
	Origin    string `json:"-"` // manual | import | drift
}

var (
	ErrNoPendingChange = errors.New("snippet has no pending change")
	// ErrLuaScriptNotLicensed 表示授权不包含 lua，不能提交或批准 Lua 脚本。
	ErrLuaScriptNotLicensed = errors.New("LICENSE_FEATURE_DENIED: 当前授权不包含 Lua（lua），不能使用自定义 Lua 脚本")
	httpSnippetNameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// snippetContext 组装校验上下文：站点名、组内已登记上游、允许回源网段。
func (d *Service) snippetContext(groupID, siteID uint) (render.SnippetContext, *render.Site, error) {
	ctx := render.SnippetContext{Upstreams: map[string]bool{}}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return ctx, nil, err
	}
	if ctx.AllowedNet, err = prefixes(g.AllowedCIDRs); err != nil {
		return ctx, nil, err
	}
	profile, err := groupProfile(g)
	if err != nil {
		return ctx, nil, err
	}
	lay, err := profile.Layout()
	if err != nil {
		return ctx, nil, err
	}
	ctx.SitesBase = lay.ConfRoot() + "/current/sites/"
	q := d.q()
	ups, err := q.Upstream.WithContext(bg).Where(q.Upstream.GroupID.Eq(groupID)).Find()
	if err != nil {
		return ctx, nil, err
	}
	for _, u := range ups {
		ctx.Upstreams[u.Name] = true
	}
	if siteID == 0 {
		return ctx, nil, nil
	}
	s, err := q.Site.WithContext(bg).Where(q.Site.ID.Eq(siteID), q.Site.GroupID.Eq(groupID)).First()
	if err != nil {
		return ctx, nil, store.ErrNotFound
	}
	var rs render.Site
	if err := json.Unmarshal([]byte(s.Spec), &rs); err != nil {
		return ctx, nil, err
	}
	rs.Name = s.Name
	ctx.Site = s.Name
	return ctx, &rs, nil
}

func (in *SnippetInput) slot() error {
	switch render.SnippetScope(in.Scope) {
	case render.ScopeHTTP:
		in.SiteID, in.RoutePath = 0, ""
		if !httpSnippetNameRe.MatchString(in.Name) {
			return errors.New("http 片段名只能使用小写字母、数字和 -，最长 32 个字符")
		}
	case render.ScopeServer:
		in.RoutePath, in.Name = "", ""
		if in.SiteID == 0 {
			return errors.New("server 片段需要选择站点")
		}
	case render.ScopeLocation:
		in.Name = ""
		if in.SiteID == 0 || in.RoutePath == "" {
			return errors.New("location 片段需要选择站点和路由")
		}
	case render.ScopeLua:
		if in.SiteID == 0 || in.RoutePath == "" {
			return errors.New("自定义 Lua 脚本需要选择站点和路由")
		}
		if !slices.Contains(render.LuaPhases, in.Name) {
			return fmt.Errorf("自定义 Lua 脚本的执行阶段只能是 %s", strings.Join(render.LuaPhases, "、"))
		}
	default:
		return fmt.Errorf("未知插入点 %q", in.Scope)
	}
	return nil
}

// validate 校验内容并返回规范化文本。
func (d *Service) validateSnippet(in SnippetInput) (string, []render.SnippetIssue, error) {
	ctx, site, err := d.snippetContext(in.GroupID, in.SiteID)
	if err != nil {
		return "", nil, err
	}
	sc := render.SnippetScope(in.Scope)
	if sc == render.ScopeLocation || sc == render.ScopeLua {
		var route *render.Route
		for i, r := range site.Routes {
			if r.Path == in.RoutePath {
				route = &site.Routes[i]
			}
		}
		if route == nil {
			return "", nil, fmt.Errorf("站点 %s 没有路由 %s", site.Name, in.RoutePath)
		}
		if sc == render.ScopeLua {
			if !d.featureOn("lua") {
				return "", nil, ErrLuaScriptNotLicensed
			}
			norm, err := render.ValidateLuaScript(in.Name, in.Content)
			if err == nil {
				err = render.CheckLuaRoute(*route, in.Name)
			}
			return norm, nil, err
		}
	}
	return render.ValidateSnippet(sc, in.Content, ctx)
}

func sha(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// SubmitSnippet 新建或修改片段，提交审批。issues 非空表示有不允许的指令，未保存。
func (d *Service) SubmitSnippet(in SnippetInput, user, ip string) (*store.Snippet, []render.SnippetIssue, error) {
	if err := in.slot(); err != nil {
		return nil, nil, err
	}
	norm, issues, err := d.validateSnippet(in)
	if err != nil || len(issues) > 0 {
		return nil, issues, err
	}
	if strings.TrimSpace(norm) == "" {
		return nil, nil, errors.New("片段内容为空；要删除请使用删除")
	}
	if in.Origin == "" {
		in.Origin = "manual"
	}
	q := d.q()
	sp := q.Snippet
	now := time.Now()
	row, err := sp.WithContext(bg).Where(sp.GroupID.Eq(in.GroupID), sp.SiteID.Eq(in.SiteID), sp.Scope.Eq(in.Scope),
		sp.RoutePath.Eq(in.RoutePath), sp.Name.Eq(in.Name)).First()
	if err != nil {
		row = &store.Snippet{GroupID: in.GroupID, SiteID: in.SiteID, Scope: in.Scope, RoutePath: in.RoutePath, Name: in.Name, CreatedBy: user}
	}
	row.Pending, row.PendingDelete, row.PendingBy, row.PendingAt, row.PendingNote, row.Origin = norm, false, user, &now, in.Note, in.Origin
	if err := sp.WithContext(bg).Save(row); err != nil {
		return nil, nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "snippet.submit", ObjectType: "snippet", ObjectID: fmt.Sprint(row.ID),
		Detail: map[string]any{"group": row.GroupID, "site": row.SiteID, "scope": row.Scope, "route": row.RoutePath, "name": row.Name,
			"sha256": sha(norm), "origin": in.Origin, "note": in.Note, "diff": textdiff.Unified("snippet", row.Content, norm)}})
	return row, nil, nil
}

// SubmitSnippetDelete 申请删除片段（批准后删除）。
func (d *Service) SubmitSnippetDelete(id uint, user, note, ip string) error {
	sp := d.q().Snippet
	now := time.Now()
	res, err := sp.WithContext(bg).Where(sp.ID.Eq(id)).UpdateSimple(sp.Pending.Value(""), sp.PendingDelete.Value(true),
		sp.PendingBy.Value(user), sp.PendingAt.Value(now), sp.PendingNote.Value(note))
	if err != nil {
		return err
	}
	if res.RowsAffected != 1 {
		return store.ErrNotFound
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "snippet.submit_delete", ObjectType: "snippet", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"note": note}})
}

// GetSnippet 返回片段。
func (d *Service) GetSnippet(id uint) (*store.Snippet, error) {
	sp := d.q().Snippet
	row, err := sp.WithContext(bg).Where(sp.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	return row, nil
}

// ListSnippets 列出组内片段（groupID 为 0 表示全部）。
func (d *Service) ListSnippets(groupID uint) ([]*store.Snippet, error) {
	sp := d.q().Snippet
	sel := sp.WithContext(bg).Order(sp.GroupID, sp.SiteID, sp.Scope, sp.RoutePath, sp.Name)
	if groupID != 0 {
		sel = sel.Where(sp.GroupID.Eq(groupID))
	}
	return sel.Find()
}

func pendingOf(row *store.Snippet) bool { return row.PendingBy != "" }

// ApproveSnippet 批准待审批的变更（批准人 ≠ 提交人）。批准时按当前上下文重新校验一次。
func (d *Service) ApproveSnippet(id uint, user, ip string) error {
	row, err := d.GetSnippet(id)
	if err != nil {
		return err
	}
	if !pendingOf(row) {
		return ErrNoPendingChange
	}
	if row.PendingBy == user {
		return ErrSelfApproval
	}
	sp := d.q().Snippet
	old := row.Content
	if row.PendingDelete {
		res, err := sp.WithContext(bg).Where(sp.ID.Eq(id), sp.PendingBy.Eq(row.PendingBy), sp.PendingDelete.Is(true)).Delete()
		if err != nil {
			return err
		}
		if res.RowsAffected != 1 {
			return ErrNoPendingChange
		}
	} else {
		norm, issues, err := d.validateSnippet(SnippetInput{GroupID: row.GroupID, SiteID: row.SiteID, Scope: row.Scope, RoutePath: row.RoutePath, Name: row.Name, Content: row.Pending})
		if err != nil {
			return err
		}
		if len(issues) > 0 {
			return fmt.Errorf("片段已不再合法（%s），请驳回后重新提交", issues[0])
		}
		now := time.Now()
		res, err := sp.WithContext(bg).Where(sp.ID.Eq(id), sp.PendingBy.Eq(row.PendingBy), sp.Pending.Eq(row.Pending)).UpdateSimple(
			sp.Content.Value(norm), sp.ContentSHA256.Value(sha(norm)), sp.ApprovedBy.Value(user), sp.ApprovedAt.Value(now),
			sp.Pending.Value(""), sp.PendingDelete.Value(false), sp.PendingBy.Value(""), sp.PendingNote.Value(""), sp.PendingAt.Null())
		if err != nil {
			return err
		}
		if res.RowsAffected != 1 {
			return ErrNoPendingChange
		}
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "snippet.approve", ObjectType: "snippet", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"submitted_by": row.PendingBy, "delete": row.PendingDelete, "sha256": sha(row.Pending),
			"diff": textdiff.Unified("snippet", old, row.Pending)}})
}

// clearPending 返回清空待审批字段的赋值（生成的查询类型未导出，按字段传入）。
func clearPending(pending, by, note field.String, del field.Bool, at field.Time) []field.AssignExpr {
	return []field.AssignExpr{pending.Value(""), del.Value(false), by.Value(""), note.Value(""), at.Null()}
}

// RejectSnippet 驳回待审批的变更；新建的片段被驳回后整条删除。
func (d *Service) RejectSnippet(id uint, user, comment, ip string) error {
	return d.dropPending(id, user, comment, ip, "snippet.reject", false)
}

// WithdrawSnippet 提交人撤回自己的变更。
func (d *Service) WithdrawSnippet(id uint, user, ip string) error {
	return d.dropPending(id, user, "", ip, "snippet.withdraw", true)
}

func (d *Service) dropPending(id uint, user, comment, ip, action string, own bool) error {
	row, err := d.GetSnippet(id)
	if err != nil {
		return err
	}
	if !pendingOf(row) {
		return ErrNoPendingChange
	}
	if own && row.PendingBy != user {
		return ErrForbidden
	}
	if !own && row.PendingBy == user {
		return ErrSelfApproval
	}
	sp := d.q().Snippet
	if row.Content == "" {
		_, err = sp.WithContext(bg).Where(sp.ID.Eq(id), sp.PendingBy.Eq(row.PendingBy)).Delete()
	} else {
		_, err = sp.WithContext(bg).Where(sp.ID.Eq(id), sp.PendingBy.Eq(row.PendingBy)).UpdateSimple(clearPending(sp.Pending, sp.PendingBy, sp.PendingNote, sp.PendingDelete, sp.PendingAt)...)
	}
	if err != nil {
		return err
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: action, ObjectType: "snippet", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"submitted_by": row.PendingBy, "comment": comment}})
}

// SnippetStats 统计生效片段中各指令出现的次数（规格书 12.7“反哺产品”：高频指令做成正式功能）。
func (d *Service) SnippetStats() (map[string]int, error) {
	rows, err := d.ListSnippets(0)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	var walk func([]*conf.Directive)
	walk = func(ds []*conf.Directive) {
		for _, x := range ds {
			out[x.Name]++
			walk(x.Block)
		}
	}
	for _, r := range rows {
		if r.Scope == string(render.ScopeLua) {
			continue // Lua 脚本不是配置指令
		}
		if ds, err := conf.Parse("snippet", []byte(r.Content)); err == nil {
			walk(ds)
		}
	}
	return out, nil
}

// applySnippets 把已批准的片段放进渲染输入；location 片段对应的路由已被删除时报错，避免片段被静默丢弃。
func (d *Service) applySnippets(groupID uint, in *render.Input, siteIDs map[string]uint) error {
	sp := d.q().Snippet
	rows, err := sp.WithContext(bg).Where(sp.GroupID.Eq(groupID), sp.Content.Neq("")).Order(sp.Scope, sp.Name, sp.SiteID, sp.RoutePath).Find()
	if err != nil {
		return fmt.Errorf("load snippets: %w", err)
	}
	byID := map[uint]int{}
	for i, s := range in.Sites {
		byID[siteIDs[s.Name]] = i
	}
	for _, r := range rows {
		switch render.SnippetScope(r.Scope) {
		case render.ScopeHTTP:
			in.HTTPSnippets = append(in.HTTPSnippets, render.NamedSnippet{Name: r.Name, Text: r.Content})
		case render.ScopeServer:
			i, ok := byID[r.SiteID]
			if !ok {
				continue // 站点已停用或删除：片段随站点一起不生效
			}
			in.Sites[i].ServerSnippet = r.Content
		case render.ScopeLocation:
			i, ok := byID[r.SiteID]
			if !ok {
				continue
			}
			found := false
			for j := range in.Sites[i].Routes {
				if in.Sites[i].Routes[j].Path == r.RoutePath {
					in.Sites[i].Routes[j].Snippet = r.Content
					found = true
				}
			}
			if !found {
				return fmt.Errorf("站点 %s 的路由 %s 已删除，但仍有生效的 location 片段 #%d：请先删除该片段", in.Sites[i].Name, r.RoutePath, r.ID)
			}
		case render.ScopeLua:
			i, ok := byID[r.SiteID]
			if !ok {
				continue
			}
			found := false
			for j := range in.Sites[i].Routes {
				rt := &in.Sites[i].Routes[j]
				if rt.Path == r.RoutePath {
					m := map[string]string{}
					for k, v := range rt.LuaScripts {
						m[k] = v
					}
					m[r.Name] = r.Content
					rt.LuaScripts = m
					found = true
				}
			}
			if !found {
				return fmt.Errorf("站点 %s 的路由 %s 已删除，但仍有生效的 Lua 脚本 #%d：请先删除该脚本", in.Sites[i].Name, r.RoutePath, r.ID)
			}
		}
	}
	return nil
}

// CheckSnippet 只校验不保存，返回规范化文本与问题列表。
func (d *Service) CheckSnippet(in SnippetInput) (string, []render.SnippetIssue, error) {
	if err := in.slot(); err != nil {
		return "", nil, err
	}
	return d.validateSnippet(in)
}
