-- etos.json：只读的 JSON 解析（JWT 头与载荷使用）。syedge-lua 附加包中审核过的库，不对外开放。
-- 输入长度由调用方限制；嵌套深度最多 16 层；解析失败返回 nil。
local byte, sub, find, char, tonumber = string.byte, string.sub, string.find, string.char, tonumber
local concat = table.concat

local M = {}

local MAX_DEPTH = 16

local function utf8(cp)
    if cp < 0x80 then
        return char(cp)
    elseif cp < 0x800 then
        return char(0xC0 + math.floor(cp / 0x40), 0x80 + cp % 0x40)
    elseif cp < 0x10000 then
        return char(0xE0 + math.floor(cp / 0x1000), 0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
    end
    return char(0xF0 + math.floor(cp / 0x40000), 0x80 + math.floor(cp / 0x1000) % 0x40,
                0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
end

local escapes = { [34] = '"', [92] = "\\", [47] = "/", [98] = "\b", [102] = "\f", [110] = "\n", [114] = "\r", [116] = "\t" }

local value

local function skip(s, i)
    local _, e = find(s, "^[ \t\r\n]*", i)
    return e + 1
end

local function str(s, i)
    -- s[i] == '"'
    local out, n = {}, 0
    i = i + 1
    while true do
        local j = find(s, '["\\]', i)
        if not j then return nil end
        if j > i then
            local chunk = sub(s, i, j - 1)
            if find(chunk, "[%z\1-\31]") then return nil end
            n = n + 1; out[n] = chunk
        end
        local c = byte(s, j)
        if c == 34 then
            return concat(out), j + 1
        end
        local e = byte(s, j + 1)
        if e == 117 then -- \uXXXX
            local hex = sub(s, j + 2, j + 5)
            if not find(hex, "^%x%x%x%x$") then return nil end
            local cp = tonumber(hex, 16)
            local nexti = j + 6
            if cp >= 0xD800 and cp <= 0xDBFF then
                local lo = sub(s, j + 6, j + 11)
                if not find(lo, "^\\u[Dd][C-Fc-f]%x%x$") then return nil end
                cp = 0x10000 + (cp - 0xD800) * 0x400 + (tonumber(sub(lo, 3), 16) - 0xDC00)
                nexti = j + 12
            elseif cp >= 0xDC00 and cp <= 0xDFFF then
                return nil
            end
            n = n + 1; out[n] = utf8(cp)
            i = nexti
        else
            local r = e and escapes[e]
            if not r then return nil end
            n = n + 1; out[n] = r
            i = j + 2
        end
    end
end

local function num(s, i)
    local _, e = find(s, "^-?%d+%.?%d*[eE]?[-+]?%d*", i)
    if not e then return nil end
    local v = tonumber(sub(s, i, e))
    if not v then return nil end
    return v, e + 1
end

local function arr(s, i, depth)
    local out, n = {}, 0
    i = skip(s, i + 1)
    if byte(s, i) == 93 then return out, i + 1 end
    while true do
        local v
        v, i = value(s, i, depth)
        if i == nil then return nil end
        n = n + 1; out[n] = v
        i = skip(s, i)
        local c = byte(s, i)
        if c == 93 then return out, i + 1 end
        if c ~= 44 then return nil end
        i = skip(s, i + 1)
    end
end

local function obj(s, i, depth)
    local out = {}
    i = skip(s, i + 1)
    if byte(s, i) == 125 then return out, i + 1 end
    while true do
        if byte(s, i) ~= 34 then return nil end
        local k
        k, i = str(s, i)
        if not k then return nil end
        i = skip(s, i)
        if byte(s, i) ~= 58 then return nil end
        local v
        v, i = value(s, skip(s, i + 1), depth)
        if i == nil then return nil end
        out[k] = v
        i = skip(s, i)
        local c = byte(s, i)
        if c == 125 then return out, i + 1 end
        if c ~= 44 then return nil end
        i = skip(s, i + 1)
    end
end

value = function(s, i, depth)
    if depth > MAX_DEPTH then return nil, nil end
    local c = byte(s, i)
    if c == 123 then return obj(s, i, depth + 1)
    elseif c == 91 then return arr(s, i, depth + 1)
    elseif c == 34 then
        local v, j = str(s, i)
        return v, j
    elseif c == 116 and sub(s, i, i + 3) == "true" then return true, i + 4
    elseif c == 102 and sub(s, i, i + 4) == "false" then return false, i + 5
    elseif c == 110 and sub(s, i, i + 3) == "null" then return M.null, i + 4
    end
    return num(s, i)
end

-- null 在结果中用这个哨兵表示，避免与“没有这个字段”混淆
M.null = setmetatable({}, { __tostring = function() return "null" end })

function M.decode(s)
    if type(s) ~= "string" or #s == 0 or #s > 16384 then return nil end
    local ok, v, i = pcall(value, s, skip(s, 1), 0)
    if not ok or i == nil or skip(s, i) <= #s then return nil end
    return v
end

return M
