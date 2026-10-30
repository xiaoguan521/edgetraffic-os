-- etos.init：syapp-lua 附加包的初始化（init_by_lua 中由渲染器生成的配置调用）。
--
-- 1. 按规则读取版本目录中的密钥文件，交给 etos.jwt；
-- 2. 收紧运行环境：之后执行的任何 Lua 代码都不能读写文件、执行命令、加载新代码或 C 模块，
--    也不能拿到 FFI。规则代码只由渲染器的模板生成，这里是纵深防御。
local jwt = require "etos.jwt"

local M = {}

local ALLOWED = { "^etos%.", "^resty%.", "^ngx%.", "^table%.", "^string%.buffer$" }

local function sandbox()
    local real_require = require
    local loaded = package.loaded

    -- 文件、进程与代码加载
    io = nil
    loaded.io = nil
    for _, k in ipairs({ "execute", "exit", "remove", "rename", "tmpname", "getenv", "setlocale" }) do
        os[k] = nil
    end
    load, loadstring, loadfile, dofile = nil, nil, nil, nil
    string.dump = nil

    -- 只保留 preload 与 Lua 文件查找器（package.path 已限定为附加包的 lualib 目录），去掉 C 模块加载
    package.loadlib = nil
    package.cpath = ""
    local loaders = package.loaders or package.searchers
    for i = #loaders, 3, -1 do
        loaders[i] = nil
    end

    -- FFI 与 debug（可借 upvalue 取回被移除的函数）不再对规则代码可见
    loaded.ffi = nil
    loaded.debug = nil
    debug = nil

    require = function(name)
        if type(name) == "string" then
            for _, p in ipairs(ALLOWED) do
                if string.find(name, p) then
                    return real_require(name)
                end
            end
        end
        error("etos: module not allowed: " .. tostring(name), 2)
    end
end

-- setup 登记全部 JWT 规则并收紧环境。cfg：规则 ID → { alg, key_file, source, cookie, iss, aud, leeway, subject_header }
function M.setup(cfg)
    if M.done then
        error("etos.init: setup called twice")
    end
    for id, c in pairs(cfg) do
        local f, err = io.open(c.key_file, "rb")
        if not f then
            error("etos.init: cannot read key for " .. tostring(id) .. ": " .. tostring(err))
        end
        local key = f:read("*a")
        f:close()
        jwt.add(id, c, key)
    end
    sandbox()
    M.done = true
end

return M
