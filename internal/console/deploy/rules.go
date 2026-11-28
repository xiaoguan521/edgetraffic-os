package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 规则积木（P10）在控制台侧的处理：JWT 密钥加密保存、IP 集加载与 Lua 附加包门禁。

// ErrLuaNotLicensed 表示站点有 JWT 规则，但授权不包含 lua。
var ErrLuaNotLicensed = errors.New("LICENSE_FEATURE_DENIED: 当前授权不包含 Lua（lua），请去掉站点的 JWT 规则与自定义 Lua 脚本")

// SealRules 在保存站点前加密 JWT 密钥：新填的密钥加密进 SecretEnc 后清空明文；没填时沿用旧站点同名规则的密钥。
// 调用后 nw 可以直接落库（不含任何明文密钥）。
func (d *Service) SealRules(nw *render.Site, old *render.Site) error {
	prev := map[string]string{}
	if old != nil {
		for _, r := range old.Rules {
			if r.Action.JWT != nil && r.Action.JWT.SecretEnc != "" {
				prev[r.Name] = r.Action.JWT.SecretEnc
			}
		}
	}
	for i := range nw.Rules {
		j := nw.Rules[i].Action.JWT
		if j == nil {
			continue
		}
		cp := *j
		cp.HasSecret = false
		switch {
		case nw.Rules[i].Action.Type != "jwt" || cp.Alg != "HS256":
			cp.Secret, cp.SecretEnc = "", ""
		case cp.Secret != "":
			if d.Box == nil {
				return errors.New("secret box not configured")
			}
			enc, err := d.Box.Seal(cp.Secret)
			if err != nil {
				return err
			}
			cp.Secret, cp.SecretEnc = "", enc
		default:
			cp.SecretEnc = prev[nw.Rules[i].Name]
		}
		nw.Rules[i].Action.JWT = &cp
	}
	return nil
}

// OpenRules 为渲染解密 JWT 密钥。会复制规则切片，不改动调用方保存的站点数据。
func (d *Service) OpenRules(s *render.Site) error {
	if len(s.Rules) == 0 {
		return nil
	}
	rules := make([]render.Rule, len(s.Rules))
	copy(rules, s.Rules)
	for i := range rules {
		j := rules[i].Action.JWT
		if j == nil {
			continue
		}
		cp := *j
		if cp.Alg == "HS256" && cp.SecretEnc != "" && cp.Secret == "" {
			if d.Box == nil {
				return errors.New("secret box not configured")
			}
			sec, err := d.Box.Open(cp.SecretEnc)
			if err != nil {
				return fmt.Errorf("rule %s: cannot decrypt the JWT secret: %w", rules[i].Name, err)
			}
			cp.Secret = sec
		}
		rules[i].Action.JWT = &cp
	}
	s.Rules = rules
	return nil
}

// RedactRules 去掉 API 响应中的密钥（明文与密文都不回显），只标明是否已设置。
func RedactRules(s *render.Site) {
	for i := range s.Rules {
		if j := s.Rules[i].Action.JWT; j != nil {
			cp := *j
			cp.HasSecret = cp.SecretEnc != ""
			cp.Secret, cp.SecretEnc = "", ""
			s.Rules[i].Action.JWT = &cp
		}
	}
}

// IPSets 返回节点组的 IP 集（渲染用）：合并跨组全局 IP 集（group_id=0）与节点组内 IP 集，同名时组内覆盖全局。
func (d *Service) IPSets(groupID uint) ([]render.IPSet, error) {
	t := d.q().IPSet
	rows, err := t.WithContext(bg).Where(t.GroupID.In(0, groupID)).Order(t.GroupID.Asc(), t.Name.Asc()).Find()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]render.IPSet, len(rows))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		var cidrs []string
		if err := json.Unmarshal([]byte(r.CIDRs), &cidrs); err != nil {
			return nil, fmt.Errorf("ip set %s: %w", r.Name, err)
		}
		if _, exists := byName[r.Name]; !exists {
			names = append(names, r.Name)
		}
		byName[r.Name] = render.IPSet{Name: r.Name, CIDRs: cidrs}
	}
	sort.Strings(names)
	out := make([]render.IPSet, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out, nil
}

// CheckLua：含 JWT 规则或自定义 Lua 脚本的版本只能下发到装有 syedge-lua 附加包、且附加包与基础变体匹配的节点。
func (d *Service) CheckLua(groupID uint) error {
	if !d.featureOn("lua") {
		return ErrLuaNotLicensed
	}
	nd := d.q().Node
	nodes, err := nd.WithContext(bg).Where(nd.GroupID.Eq(groupID), nd.Status.Neq("revoked")).Order(nd.Name).Find()
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range nodes {
		want := "/standard"
		if n.GM() {
			want = "/gmssl"
		}
		if n.LuaAddon == "" || !strings.HasSuffix(n.LuaAddon, want) {
			missing = append(missing, n.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("LUA_ADDON_REQUIRED: 站点有 JWT 规则或自定义 Lua 脚本，但节点 %s 没有安装与当前 %s 匹配的 %s 附加包；请先在节点上安装附加包（见 docs/lua.md）", strings.Join(missing, "、"), brand.ID(), brand.Lua())
	}
	return nil
}

// HasLuaSites 报告节点组是否有启用 JWT 规则的站点。
func (d *Service) HasLuaSites(groupID uint) bool {
	q := d.q().Site
	sites, _ := q.WithContext(bg).Where(q.GroupID.Eq(groupID), q.Enabled.Is(true)).Find()
	for _, s := range sites {
		var rs render.Site
		if json.Unmarshal([]byte(s.Spec), &rs) != nil {
			continue
		}
		for _, r := range rs.Rules {
			if r.Action.Type == "jwt" {
				return true
			}
		}
	}
	return false
}

// luaNodes 返回节点组中装有附加包的节点（升级时需要附加包）。
func luaNodes(nodes []*store.Node) []*store.Node {
	var out []*store.Node
	for _, n := range nodes {
		if n.LuaAddon != "" {
			out = append(out, n)
		}
	}
	return out
}
