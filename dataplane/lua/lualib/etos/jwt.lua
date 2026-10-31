-- etos.jwt：JWT 校验（HS256 / RS256）。syapp-lua 附加包中审核过的库，只由渲染器生成的配置调用。
--
-- 密码学运算通过 FFI 调用 syapp 自身导出的 OpenSSL / 铜锁函数（基础包以 -Wl,-u 固定导出这些符号，
-- CI 校验附加包需要的每个符号都存在）。只接受配置的算法（拒绝 alg=none 与算法混淆），
-- 必须有 exp；校验 nbf / iss / aud；签名比较用 CRYPTO_memcmp（常量时间）。
-- 校验通过的令牌按 worker 缓存，直到过期（最多 60 秒）。
local ffi = require "ffi"
local lrucache = require "resty.lrucache"
local json = require "etos.json"

local C = ffi.C
local ngx = ngx
local ngx_var, ngx_time, ngx_log, ngx_exit = ngx.var, ngx.time, ngx.log, ngx.exit
local decode_base64 = ngx.decode_base64
local type, tostring, pairs = type, tostring, pairs
local sub, find, gsub, rep = string.sub, string.find, string.gsub, string.rep

local decls = {
    "typedef struct evp_md_st EVP_MD;",
    "typedef struct evp_md_ctx_st EVP_MD_CTX;",
    "typedef struct evp_pkey_st EVP_PKEY;",
    "typedef struct evp_pkey_ctx_st EVP_PKEY_CTX;",
    "typedef struct bio_st BIO;",
    "typedef struct engine_st ENGINE;",
    "const EVP_MD *EVP_sha256(void);",
    "unsigned char *HMAC(const EVP_MD *evp_md, const char *key, int key_len, const char *d, size_t n, unsigned char *md, unsigned int *md_len);",
    "int CRYPTO_memcmp(const void *a, const char *b, size_t len);",
    "BIO *BIO_new_mem_buf(const char *buf, int len);",
    "int BIO_free(BIO *a);",
    "EVP_PKEY *PEM_read_bio_PUBKEY(BIO *bp, EVP_PKEY **x, void *cb, void *u);",
    "void EVP_PKEY_free(EVP_PKEY *pkey);",
    "EVP_MD_CTX *EVP_MD_CTX_new(void);",
    "void EVP_MD_CTX_free(EVP_MD_CTX *ctx);",
    "int EVP_DigestVerifyInit(EVP_MD_CTX *ctx, EVP_PKEY_CTX **pctx, const EVP_MD *type, ENGINE *e, EVP_PKEY *pkey);",
    "int EVP_DigestVerify(EVP_MD_CTX *ctx, const char *sigret, size_t siglen, const char *tbs, size_t tbslen);",
    "void ERR_clear_error(void);",
}
for _, d in ipairs(decls) do
    -- 其他库可能已经声明过同名类型 / 函数：重复声明时忽略
    pcall(ffi.cdef, d)
end

local M = {}

local rules = {}
local CACHE_SIZE = 10000
local md_out = ffi.new("unsigned char[32]")
local md_len = ffi.new("unsigned int[1]")
local MAX_TOKEN = 8192

local function b64url(s)
    s = gsub(gsub(s, "%-", "+"), "_", "/")
    local pad = #s % 4
    if pad == 1 then return nil end
    if pad > 0 then s = s .. rep("=", 4 - pad) end
    return decode_base64(s)
end

local function load_pubkey(pem)
    local bio = C.BIO_new_mem_buf(pem, #pem)
    if bio == nil then return nil end
    local pkey = C.PEM_read_bio_PUBKEY(bio, nil, nil, nil)
    C.BIO_free(bio)
    if pkey == nil then
        C.ERR_clear_error()
        return nil
    end
    return ffi.gc(pkey, C.EVP_PKEY_free)
end

-- add 在 init 阶段由 etos.init 调用：登记一条规则及其密钥材料。
function M.add(id, cfg, key)
    if type(id) ~= "string" or type(cfg) ~= "table" or type(key) ~= "string" or #key == 0 then
        error("etos.jwt: bad rule " .. tostring(id))
    end
    local r = {
        alg = cfg.alg, source = cfg.source, cookie = cfg.cookie, iss = cfg.iss, aud = cfg.aud,
        leeway = tonumber(cfg.leeway) or 0, subject_header = cfg.subject_header,
    }
    if r.alg == "HS256" then
        r.secret = key
    elseif r.alg == "RS256" then
        r.pkey = load_pubkey(key)
        if not r.pkey then error("etos.jwt: cannot load the RS256 public key of " .. id) end
    else
        error("etos.jwt: unsupported alg for " .. id)
    end
    if r.source == "cookie" then
        if not find(r.cookie or "", "^[%w_]+$") then error("etos.jwt: bad cookie name for " .. id) end
        r.cookie_var = "cookie_" .. r.cookie
    end
    r.cache = lrucache.new(CACHE_SIZE) -- 每条规则独立缓存，键就是令牌本身（不拼接字符串）
    rules[id] = r
end

local function verify_hs256(r, signed, sig)
    if #sig ~= 32 then return false end
    local out = C.HMAC(C.EVP_sha256(), r.secret, #r.secret, signed, #signed, md_out, md_len)
    if out == nil or md_len[0] ~= 32 then return false end
    return C.CRYPTO_memcmp(md_out, sig, 32) == 0
end

local function verify_rs256(r, signed, sig)
    local ctx = C.EVP_MD_CTX_new()
    if ctx == nil then return false end
    local ok = C.EVP_DigestVerifyInit(ctx, nil, C.EVP_sha256(), nil, r.pkey) == 1
        and C.EVP_DigestVerify(ctx, sig, #sig, signed, #signed) == 1
    C.EVP_MD_CTX_free(ctx)
    if not ok then C.ERR_clear_error() end
    return ok
end

local function aud_ok(want, aud)
    if type(aud) == "string" then return aud == want end
    if type(aud) == "table" then
        for _, v in pairs(aud) do
            if v == want then return true end
        end
    end
    return false
end

local function deny(r, why)
    ngx_log(ngx.INFO, "jwt rejected: ", why)
    ngx.header["WWW-Authenticate"] = 'Bearer error="invalid_token"'
    return ngx_exit(401)
end

local function set_subject(r, sub_claim)
    if r.subject_header ~= "" then
        if type(sub_claim) == "string" and #sub_claim <= 256 and not find(sub_claim, "[%z\1-\31\127]") then
            ngx.req.set_header(r.subject_header, sub_claim)
        end
    end
end

-- check 校验当前请求；失败时直接返回 401 结束请求。
function M.check(id)
    local r = rules[id]
    if not r then
        ngx_log(ngx.ERR, "etos.jwt: unknown rule ", id)
        return ngx_exit(500)
    end
    if r.subject_header ~= "" then
        ngx.req.clear_header(r.subject_header) -- 客户端不能伪造转发给上游的身份头
    end
    -- 快速路径：缓存键直接用取到的原始值（Authorization 头或 Cookie），命中时不再切分字符串
    local raw
    if r.cookie_var then
        raw = ngx_var[r.cookie_var]
    else
        raw = ngx_var.http_authorization
    end
    if not raw or #raw > MAX_TOKEN then
        return deny(r, "missing token")
    end
    local hit = r.cache:get(raw)
    if hit then
        set_subject(r, hit)
        return true
    end
    local token = raw
    if not r.cookie_var then
        local p = sub(raw, 1, 7)
        if p ~= "Bearer " and p ~= "bearer " and p ~= "BEARER " then
            return deny(r, "missing token")
        end
        token = sub(raw, 8)
    end
    if not find(token, "^[%w%-_%.]+$") then return deny(r, "malformed token") end
    local now = ngx_time()
    local _, _, h64, p64, s64 = find(token, "^([%w%-_]+)%.([%w%-_]+)%.([%w%-_]+)$")
    if not h64 then return deny(r, "malformed token") end
    local header = json.decode(b64url(h64) or "")
    if type(header) ~= "table" or header.alg ~= r.alg then
        return deny(r, "unexpected alg")
    end
    local sig = b64url(s64)
    if not sig then return deny(r, "bad signature encoding") end
    local signed = h64 .. "." .. p64
    local ok
    if r.alg == "HS256" then ok = verify_hs256(r, signed, sig) else ok = verify_rs256(r, signed, sig) end
    if not ok then return deny(r, "bad signature") end

    local claims = json.decode(b64url(p64) or "")
    if type(claims) ~= "table" then return deny(r, "bad claims") end
    local exp, nbf = claims.exp, claims.nbf
    if type(exp) ~= "number" then return deny(r, "missing exp") end
    if now > exp + r.leeway then return deny(r, "expired") end
    if nbf ~= nil and (type(nbf) ~= "number" or now + r.leeway < nbf) then return deny(r, "not yet valid") end
    if r.iss ~= "" and claims.iss ~= r.iss then return deny(r, "issuer mismatch") end
    if r.aud ~= "" and not aud_ok(r.aud, claims.aud) then return deny(r, "audience mismatch") end

    local ttl = exp - now
    if ttl > 60 then ttl = 60 end
    if ttl > 0 then r.cache:set(raw, type(claims.sub) == "string" and claims.sub or "", ttl) end
    set_subject(r, claims.sub)
    return true
end

return M
