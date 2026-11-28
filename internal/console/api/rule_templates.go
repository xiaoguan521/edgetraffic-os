package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

const ruleTemplatesKVKey = "rule_templates"

// RuleTemplate 是跨节点组复用的公共访问规则模板（不含任何 JWT 密钥）。
type RuleTemplate struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Rules       []render.Rule `json:"rules"`
	UpdatedBy   string        `json:"updated_by"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

func (s *Server) loadRuleTemplates() ([]RuleTemplate, error) {
	raw, err := s.Store.GetKV(ruleTemplatesKVKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return []RuleTemplate{}, nil
	}
	var list []RuleTemplate
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (s *Server) saveRuleTemplatesList(list []RuleTemplate) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.Store.SetKV(ruleTemplatesKVKey, string(b))
}

func (s *Server) listRuleTemplates(c *gin.Context) {
	s.ruleTplMu.Lock()
	list, err := s.loadRuleTemplates()
	s.ruleTplMu.Unlock()
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func validateTemplateRule(r *render.Rule) error {
	if err := render.ValidateName(r.Name); err != nil {
		return fmt.Errorf("规则名不合法：%s", r.Name)
	}
	if len(r.Conditions) > render.MaxConditions {
		return fmt.Errorf("规则 %s 条件数超过上限 %d", r.Name, render.MaxConditions)
	}
	for i := range r.Conditions {
		cd := &r.Conditions[i]
		switch cd.Kind {
		case "ip":
			norm, err := render.NormalizeCIDRs(cd.CIDRs, render.MaxRuleCIDRs)
			if err != nil {
				return fmt.Errorf("规则 %s IP 条件不合法：%v", r.Name, err)
			}
			cd.CIDRs = norm
		case "ip_set":
			if err := render.ValidateName(cd.Set); err != nil {
				return fmt.Errorf("规则 %s IP 集名不合法：%s", r.Name, cd.Set)
			}
		case "path":
			switch cd.Op {
			case "prefix", "exact":
				if err := render.ValidatePath(cd.Value); err != nil {
					return fmt.Errorf("规则 %s 路径条件不合法：%v", r.Name, err)
				}
			case "regex":
				if cd.Value == "" || len(cd.Value) > 256 {
					return fmt.Errorf("规则 %s 路径正则为空或过长", r.Name)
				}
				if _, err := regexp.Compile(cd.Value); err != nil {
					return fmt.Errorf("规则 %s 路径正则语法错误：%v", r.Name, err)
				}
			default:
				return fmt.Errorf("规则 %s 路径匹配方式不合法：%s", r.Name, cd.Op)
			}
		case "header", "user_agent":
			if cd.Kind == "header" && strings.TrimSpace(cd.Header) == "" {
				return fmt.Errorf("规则 %s 缺少请求头名称", r.Name)
			}
			switch cd.Op {
			case "exists":
				if cd.Kind != "header" {
					return fmt.Errorf("规则 %s 仅请求头支持 exists 匹配", r.Name)
				}
			case "equals", "prefix", "contains":
				if strings.TrimSpace(cd.Value) == "" || len(cd.Value) > 256 {
					return fmt.Errorf("规则 %s 匹配值为空或过长", r.Name)
				}
			case "regex":
				if cd.Value == "" || len(cd.Value) > 256 {
					return fmt.Errorf("规则 %s 正则为空或过长", r.Name)
				}
				if _, err := regexp.Compile(cd.Value); err != nil {
					return fmt.Errorf("规则 %s 正则语法错误：%v", r.Name, err)
				}
			default:
				return fmt.Errorf("规则 %s 匹配方式不合法：%s", r.Name, cd.Op)
			}
		case "method":
			if len(cd.Methods) == 0 {
				return fmt.Errorf("规则 %s 至少需指定一个请求方法", r.Name)
			}
		default:
			return fmt.Errorf("规则 %s 未知条件类型：%s", r.Name, cd.Kind)
		}
	}
	switch r.Action.Type {
	case "deny":
		switch r.Action.Status {
		case 401, 403, 404, 429, 444, 451, 503:
		default:
			return fmt.Errorf("规则 %s 拒绝状态码不支持：%d", r.Name, r.Action.Status)
		}
	case "redirect":
		switch r.Action.Status {
		case 301, 302, 307, 308:
		default:
			return fmt.Errorf("规则 %s 跳转状态码不支持：%d", r.Name, r.Action.Status)
		}
		u, err := url.Parse(r.Action.To)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("规则 %s 跳转地址无效", r.Name)
		}
	case "rate_limit":
		if r.Action.Rate <= 0 || (r.Action.Per != "s" && r.Action.Per != "m") {
			return fmt.Errorf("规则 %s 限速参数无效", r.Name)
		}
	case "auth_request":
		if r.Action.Auth == nil || strings.TrimSpace(r.Action.Auth.Path) == "" {
			return fmt.Errorf("规则 %s 缺少外部鉴权配置", r.Name)
		}
	case "jwt":
		if r.Action.JWT == nil {
			return fmt.Errorf("规则 %s 缺少 JWT 配置", r.Name)
		}
	default:
		return fmt.Errorf("规则 %s 未知动作类型：%s", r.Name, r.Action.Type)
	}
	return nil
}

func (s *Server) saveRuleTemplate(c *gin.Context) {
	var req struct {
		Name        string        `json:"name"`
		Description string        `json:"description"`
		Rules       []render.Rule `json:"rules"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := render.ValidateName(name); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", "模板名称只能使用小写字母、数字和连字符")
		return
	}
	desc := strings.TrimSpace(req.Description)
	if len(desc) > 200 {
		fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", "说明不能超过 200 个字符")
		return
	}
	if len(req.Rules) == 0 || len(req.Rules) > 32 {
		fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", "规则模板需包含 1–32 条访问规则")
		return
	}
	// 公共模板绝不保存任何密钥（JWT 规则仅保留结构，套用到站点后再填密钥）
	cleanRules := make([]render.Rule, len(req.Rules))
	seenRule := map[string]bool{}
	for i, r := range req.Rules {
		if seenRule[r.Name] {
			fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", "模板内规则名重复："+r.Name)
			return
		}
		seenRule[r.Name] = true
		if err := validateTemplateRule(&r); err != nil {
			fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", err.Error())
			return
		}
		if r.Action.JWT != nil {
			cp := *r.Action.JWT
			cp.Secret, cp.SecretEnc, cp.HasSecret = "", "", false
			r.Action.JWT = &cp
		}
		cleanRules[i] = r
	}

	s.ruleTplMu.Lock()
	defer s.ruleTplMu.Unlock()

	list, err := s.loadRuleTemplates()
	if err != nil {
		failErr(c, err)
		return
	}
	item := RuleTemplate{
		Name:        name,
		Description: desc,
		Rules:       cleanRules,
		UpdatedBy:   c.GetString("user"),
		UpdatedAt:   time.Now().UTC(),
	}
	found := false
	for i := range list {
		if list[i].Name == name {
			list[i] = item
			found = true
			break
		}
	}
	if !found {
		if len(list) >= 100 {
			fail(c, http.StatusBadRequest, "INVALID_TEMPLATE", "最多保存 100 个公共规则模板")
			return
		}
		list = append(list, item)
	}
	if err := s.saveRuleTemplatesList(list); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "rule_template.save", "rule_template", name, map[string]any{"rules": len(cleanRules)})
	c.JSON(http.StatusOK, item)
}

func (s *Server) deleteRuleTemplate(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	s.ruleTplMu.Lock()
	defer s.ruleTplMu.Unlock()

	list, err := s.loadRuleTemplates()
	if err != nil {
		failErr(c, err)
		return
	}
	next := make([]RuleTemplate, 0, len(list))
	removed := false
	for _, item := range list {
		if item.Name == name {
			removed = true
			continue
		}
		next = append(next, item)
	}
	if !removed {
		fail(c, http.StatusNotFound, "NOT_FOUND", "模板不存在")
		return
	}
	if err := s.saveRuleTemplatesList(next); err != nil {
		failErr(c, err)
		return
	}
	s.audit(c, "rule_template.delete", "rule_template", name, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
