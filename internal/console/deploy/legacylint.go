package deploy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// legacyWarning 是提交原生兼容包时的静态提示。它只读包内的文本，不执行、不改写，
// 也不阻止提交：包里的配置本来就不经 internal/render，这里靠文本识别，会有漏报与误报，
// 目的是在审批前把已知的常见错误摆到审批人面前。
type legacyWarning struct {
	Code    string `json:"code"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

const (
	warnVersionedPath = "lua-versioned-path"
	warnNoPackagePath = "lua-package-path-missing"
	warnGlobalNotInit = "lua-global-uninitialized"

	legacyWarningNoteItems = 3
	legacyAuditWarnings    = 20
)

var (
	// 附加包的稳定入口是 modules/lua；modules/lua-<版本>/ 会在附加包升级后消失。
	luaVersionedDir  = regexp.MustCompile(`modules/lua-[0-9][^/;"'\s]*/`)
	luaPathDirective = regexp.MustCompile(`^\s*lua_package_c?path\b`)
	luaPackagePath   = regexp.MustCompile(`^\s*lua_package_path\b`)
	luaByLua         = regexp.MustCompile(`\b[a-z_]*by_lua[a-z_]*\b`)

	// 常见的“由 init_*_by_lua 创建、location 里直接使用”的全局变量。
	knownLuaGlobals = []string{"skywalking_tracer", "prometheus", "metric_connections", "metric_requests", "metric_latency", "metric_bytes"}
)

type luaGlobalPatterns struct {
	name   string
	use    *regexp.Regexp
	assign *regexp.Regexp
	viaG   []*regexp.Regexp
}

var luaGlobals = func() []luaGlobalPatterns {
	out := make([]luaGlobalPatterns, 0, len(knownLuaGlobals))
	for _, n := range knownLuaGlobals {
		q := regexp.QuoteMeta(n)
		out = append(out, luaGlobalPatterns{
			name:   n,
			use:    regexp.MustCompile(`(^|[^\w.])` + q + `\s*:`),
			assign: regexp.MustCompile(`(^|[^\w.])(local\s+)?` + q + `\s*=[^=]`),
			viaG: []*regexp.Regexp{
				regexp.MustCompile(`_G\s*\.\s*` + q + `\s*=[^=]`),
				regexp.MustCompile(`_G\s*\[\s*["']` + q + `["']\s*\]\s*=[^=]`),
			},
		})
	}
	return out
}()

func isLuaComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "--")
}

type legacyLine struct {
	file string
	line int
	text string
}

// lintLegacyLua 检查冻结前的包内容。files 的键是包内相对路径。
func lintLegacyLua(files map[string][]byte) []legacyWarning {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var confLines, allLines []legacyLine
	for _, n := range names {
		lower := strings.ToLower(n)
		isConf, isLua := strings.HasSuffix(lower, ".conf"), strings.HasSuffix(lower, ".lua")
		if !isConf && !isLua {
			continue
		}
		for i, raw := range strings.Split(string(files[n]), "\n") {
			if isLuaComment(strings.TrimSpace(raw)) {
				continue
			}
			l := legacyLine{file: n, line: i + 1, text: raw}
			allLines = append(allLines, l)
			if isConf {
				confLines = append(confLines, l)
			}
		}
	}

	var out []legacyWarning
	hasPath := false
	var firstLua *legacyLine
	for i, l := range confLines {
		if luaPackagePath.MatchString(l.text) {
			hasPath = true
		}
		if luaPathDirective.MatchString(l.text) && luaVersionedDir.MatchString(l.text) {
			out = append(out, legacyWarning{Code: warnVersionedPath, File: l.file, Line: l.line,
				Message: "lua_package_path/cpath 指向带版本号的附加包目录，附加包升级后会失效；请改用稳定入口 modules/lua（例如 <前缀>/modules/lua/lualib/?.lua）"})
		}
		if firstLua == nil && luaByLua.MatchString(l.text) {
			firstLua = &confLines[i]
		}
	}
	if firstLua != nil && !hasPath {
		out = append(out, legacyWarning{Code: warnNoPackagePath, File: firstLua.file, Line: firstLua.line,
			Message: "配置使用了 Lua 指令但没有设置 lua_package_path，Lua 模块可能找不到 resty.core 等库，reload 时才会报错"})
	}

	for _, g := range luaGlobals {
		var first *legacyLine
		assigned := false
		for i, l := range allLines {
			if first == nil && g.use.MatchString(l.text) {
				first = &allLines[i]
			}
			if !assigned {
				for _, m := range g.assign.FindAllStringSubmatch(l.text, -1) {
					if m[2] == "" {
						assigned = true
					}
				}
				for _, re := range g.viaG {
					if re.MatchString(l.text) {
						assigned = true
					}
				}
			}
		}
		if first != nil && !assigned {
			out = append(out, legacyWarning{Code: warnGlobalNotInit, File: first.file, Line: first.line, Target: g.name,
				Message: fmt.Sprintf("%s 在使用，但整个包里没有对它赋值（缺少 init_by_lua / init_worker_by_lua 初始化），请求会报 nil value", g.name)})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Code != b.Code:
			return a.Code < b.Code
		}
		return a.Target < b.Target
	})
	return out
}

// legacyWarningNote 生成追加到发布说明里的一句话，让审批人在列表里就能看到。
func legacyWarningNote(ws []legacyWarning) string {
	if len(ws) == 0 {
		return ""
	}
	shown := make([]string, 0, legacyWarningNoteItems)
	for i, w := range ws {
		if i == legacyWarningNoteItems {
			break
		}
		shown = append(shown, fmt.Sprintf("%s %s:%d", w.Code, w.File, w.Line))
	}
	more := ""
	if len(ws) > legacyWarningNoteItems {
		more = fmt.Sprintf(" 等 %d 项", len(ws))
	}
	return fmt.Sprintf("【静态检查提示 %d 项：%s%s；仅提示，不阻止提交】", len(ws), strings.Join(shown, "、"), more)
}

// legacyWarningAudit 给审计用：只写 code 与位置，不写任何配置内容。
func legacyWarningAudit(ws []legacyWarning) []string {
	out := make([]string, 0, len(ws))
	for i, w := range ws {
		if i == legacyAuditWarnings {
			break
		}
		item := fmt.Sprintf("%s %s:%d", w.Code, w.File, w.Line)
		if w.Target != "" {
			item += " " + w.Target
		}
		out = append(out, item)
	}
	return out
}
