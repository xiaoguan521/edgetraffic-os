package render

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// 自定义 Lua 脚本（受控开放，2026-11-07 决定，见 docs/lua.md 第 5 节）。
//
// 脚本是路由（location）级的自定义片段（插入点 lua），每条路由每个执行阶段一段，由管理员提交、另一位管理员批准后才参与渲染。
// 渲染器从不把脚本文本拼进配置：脚本写成版本目录中的 lua/script-*.lua 文件，配置里只有渲染器生成的固定调用
// `<阶段>_by_lua_block { return require("etos.script").run("<编号>") }`（编号写成 \ddd 转义的字面量）。
//
// 加载与运行环境由 init_by_lua_block 中的固定代码（luaScriptRuntime）完成：
//   - 在 etos.init 收紧环境之前，从版本目录读入脚本，只按文本编译（不接受字节码）。syedge -t 不执行 init 阶段，
//     语法错误在 reload 时出现：master 不加载新配置、继续以当前配置服务，agent 从 error.log 识别
//     “init_by_lua error”后立即回滚，错误（含脚本编号与行号）出现在发布记录中；
//   - 脚本运行在专用环境中：只有 ngx（去掉 ngx.socket 等，不能绕过回源网段限制）、string（无 dump）、table、math、
//     os 的时间函数与 etos.json；不能读写文件、执行命令、加载代码或模块、使用 FFI / debug / getmetatable / rawset，
//     不能写全局变量（请求之间不共享状态），标准库与 ngx 的顶层字段只读；
//   - 运行时错误由 Lua 模块记录到 error.log 并返回 500。
// 管理员仍可写出死循环、耗尽内存或泄露请求数据的脚本：这是开放脚本的代价，由审批与授权（lua）约束。

// LuaPhases 是允许的执行阶段（插入点 lua 的片段名）。
var LuaPhases = []string{"rewrite", "access", "content", "header_filter", "body_filter"}

const maxLuaScriptBytes = 16 << 10

// ErrLuaScript 表示脚本不合法。
var ErrLuaScript = errors.New("invalid lua script")

func validPhase(p string) bool {
	for _, x := range LuaPhases {
		if x == p {
			return true
		}
	}
	return false
}

// ValidateLuaScript 校验并规范化一段脚本（统一换行、去掉行尾空白与首尾空行）。语法由节点上的 syedge -t 检查。
func ValidateLuaScript(phase, src string) (string, error) {
	if !validPhase(phase) {
		return "", fmt.Errorf("%w: 执行阶段只能是 %s", ErrLuaScript, strings.Join(LuaPhases, "、"))
	}
	if len(src) > maxLuaScriptBytes {
		return "", fmt.Errorf("%w: 超过 %d 字节", ErrLuaScript, maxLuaScriptBytes)
	}
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return "", fmt.Errorf("%w: 必须是 UTF-8 文本", ErrLuaScript)
	}
	if strings.HasPrefix(strings.TrimSpace(src), "\x1b") {
		return "", fmt.Errorf("%w: 不接受字节码", ErrLuaScript)
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	out := strings.Trim(strings.Join(lines, "\n"), "\n")
	if out == "" {
		return "", fmt.Errorf("%w: 脚本为空", ErrLuaScript)
	}
	return out + "\n", nil
}

// CheckLuaRoute 检查脚本的执行阶段对该路由是否有意义：跳转 / 拒绝路由在 rewrite 阶段就已返回，
// rewrite、access、content 阶段的脚本不会执行。
func CheckLuaRoute(r Route, phase string) error {
	if (r.Mode == "redirect" || r.Mode == "deny") && (phase == "rewrite" || phase == "access" || phase == "content") {
		return fmt.Errorf("%w: 路由 %s 是%s路由，%s 阶段的脚本不会执行（只能用 header_filter / body_filter）", ErrLuaScript, r.Path,
			map[string]string{"redirect": "跳转", "deny": "拒绝"}[r.Mode], phase)
	}
	return nil
}

// luaScript 是一段脚本在渲染时的信息。
type luaScript struct {
	ID, File, Phase string
}

func routeScripts(s Site, r Route) []luaScript {
	var out []luaScript
	for _, ph := range LuaPhases {
		if strings.TrimSpace(r.LuaScripts[ph]) == "" {
			continue
		}
		d := routeDir(r)
		out = append(out, luaScript{
			ID:    s.Name + ":" + d + ":" + ph,
			File:  "lua/script-" + s.Name + "-" + d + "-" + ph + ".lua",
			Phase: ph,
		})
	}
	return out
}

func allScripts(in Input) []luaScript {
	var out []luaScript
	for _, s := range in.Sites {
		for _, r := range s.Routes {
			out = append(out, routeScripts(s, r)...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// validateLuaScripts 在渲染副本上校验并规范化各路由的脚本。
func (in *Input) validateLuaScripts() error {
	for i := range in.Sites {
		st := &in.Sites[i]
		for j := range st.Routes {
			r := &st.Routes[j]
			if len(r.LuaScripts) == 0 {
				continue
			}
			m := map[string]string{}
			for ph, src := range r.LuaScripts {
				if strings.TrimSpace(src) == "" {
					continue
				}
				out, err := ValidateLuaScript(ph, src)
				if err == nil {
					err = CheckLuaRoute(*r, ph)
				}
				if err != nil {
					return fmt.Errorf("site %s route %s lua %s: %w", st.Name, r.Path, ph, err)
				}
				m[ph] = out
			}
			r.LuaScripts = m
		}
	}
	return nil
}

// renderLuaRoute 生成路由 location 内调用脚本的指令。
func renderLuaRoute(s Site, r Route) string {
	var b strings.Builder
	for _, sc := range routeScripts(s, r) {
		fmt.Fprintf(&b, "        %s_by_lua_block { return require(\"etos.script\").run(%s) }\n", sc.Phase, luaStr(sc.ID))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// luaScriptRuntime 在 init_by_lua_block 中、etos.init 收紧环境之前执行：编译脚本并登记 etos.script。
// 前面由渲染器写出 `local files = { [编号] = 路径, ... }`（全部为 \ddd 转义的字面量）。
const luaScriptRuntime = `
        local io_open, load, setfenv, setmetatable, getmetatable, pairs, error, tostring, type = io.open, load, setfenv, setmetatable, getmetatable, pairs, error, tostring, type
        local compiled = {}
        for id, path in pairs(files) do
            local f, err = io_open(path, "rb")
            if not f then error("lua script " .. id .. ": " .. tostring(err)) end
            local src = f:read("*a")
            f:close()
            local fn, perr = load(src, "=" .. id, "t")
            if not fn then error("lua script syntax error: " .. tostring(perr)) end
            compiled[id] = fn
        end
        local function ro(t, name)
            return setmetatable({}, {
                __index = t,
                __newindex = function(_, k) error(name .. "." .. tostring(k) .. " is read-only", 2) end,
                __metatable = false,
            })
        end
        local ngx = ngx
        local blocked = { socket = true, run_worker_thread = true }
        local writable = { status = true, ctx = true }
        -- ngx 的子表（ngx.req、ngx.re、ngx.location …）只读，避免替换其中的函数影响其他脚本与 JWT 规则；
        -- var / header / ctx / arg / shared 按请求读写，原样提供
        local passthrough = { var = true, header = true, ctx = true, arg = true, shared = true }
        local wrapped = {}
        local safe_ngx = setmetatable({}, {
            __index = function(_, k)
                if blocked[k] then return nil end
                local v = ngx[k]
                if type(v) == "table" and not passthrough[k] then
                    local w = wrapped[k]
                    if not w then w = ro(v, "ngx." .. tostring(k)) wrapped[k] = w end
                    return w
                end
                return v
            end,
            __newindex = function(_, k, v)
                if writable[k] then ngx[k] = v return end
                error("ngx." .. tostring(k) .. " is read-only", 2)
            end,
            __metatable = false,
        })
        local str = {}
        for k, v in pairs(string) do if k ~= "dump" then str[k] = v end end
        local json = ro(require("etos.json"), "etos.json")
        -- 脚本只能给自己新建的表设置元表：已有元表的表（ngx.var / ngx.header / ngx.arg / 共享字典等整个 worker 共用的表，
        -- 以及上面带 __metatable 的只读代理）不允许替换，否则一段脚本能让同一 worker 里其他站点与规则的行为失常
        local function guarded_setmetatable(t, mt)
            if getmetatable(t) ~= nil then error("cannot change a metatable that is already set", 2) end
            return setmetatable(t, mt)
        end
        local base = {
            ngx = safe_ngx, string = ro(str, "string"), table = ro(table, "table"), math = ro(math, "math"),
            os = ro({ time = os.time, date = os.date, clock = os.clock, difftime = os.difftime }, "os"),
            bit = ro(bit, "bit"), coroutine = ro(coroutine, "coroutine"),
            assert = assert, error = error, ipairs = ipairs, next = next, pairs = pairs, pcall = pcall, xpcall = xpcall,
            select = select, tonumber = tonumber, tostring = tostring, type = type, unpack = unpack, setmetatable = guarded_setmetatable,
            require = function(name)
                if name == "etos.json" then return json end
                error("module not allowed in lua scripts: " .. tostring(name), 2)
            end,
        }
        local env = setmetatable({}, {
            __index = base,
            __newindex = function(_, k) error("assignment to global '" .. tostring(k) .. "' is not allowed (use local)", 2) end,
            __metatable = false,
        })
        for _, fn in pairs(compiled) do setfenv(fn, env) end
        package.loaded["etos.script"] = {
            run = function(id)
                local fn = compiled[id]
                if type(fn) ~= "function" then error("unknown lua script " .. tostring(id)) end
                return fn()
            end,
        }
`

// renderLuaScriptsInit 生成 init 中加载脚本的代码（没有脚本时为空）。
func renderLuaScriptsInit(in Input) string {
	scripts := allScripts(in)
	if len(scripts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("        -- 自定义 Lua 脚本（经审批）：收紧环境之前编译，运行在受限环境中（docs/lua.md 第 5 节）\n        do\n        local files = {\n")
	for _, sc := range scripts {
		fmt.Fprintf(&b, "            [%s] = %s,\n", luaStr(sc.ID), luaStr(in.P().ReleasesDir+in.Revision+"/"+sc.File))
	}
	b.WriteString("        }")
	b.WriteString(luaScriptRuntime)
	b.WriteString("        end\n")
	return b.String()
}
