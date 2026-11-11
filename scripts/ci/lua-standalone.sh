#!/usr/bin/env bash
# 在一次性 CI 主机上验证独立模式“一条命令启用 Lua”（install.sh --standalone / --standalone-disable）。
#   用法：scripts/ci/lua-standalone.sh <dist-lua 目录> <standard|gmssl>
#   环境准备照 lua-acceptance.sh：基础包装到编译期前缀（生产路径），master 以 syedge 运行；附加包用发布包里的
#   install.sh / uninstall.sh，由 root 执行。CI 的基础包不带初始版本，这里用 syedge-agent package-defaults 生成
#   （与发布包相同），r000000 与 conf/current 的写法照 syedge-postinstall.sh。
#   断言编号与 docs/dev/ops-experience-handbook.md 的 T5 表格一致。
set -uo pipefail
DIST=$(realpath "$1"); BASE=$2
HERE=$(cd "$(dirname "$0")" && pwd)
ARCH=$(uname -m)
PREFIX=${SYEDGE_PREFIX:-/opt/syedge}
[[ "$BASE" == standard || "$BASE" == gmssl ]] || { echo "用法：$0 <dist-lua 目录> <standard|gmssl>" >&2; exit 2; }
SY="$PREFIX/sbin/syedge"
CONF="$PREFIX/conf/current/syedge.conf"
MAIN="$PREFIX/conf/releases/r000000/syedge.conf"
NEWCONF="$PREFIX/conf/releases/r000000/conf.d/00-lua-standalone.conf"
SITE="$PREFIX/conf/releases/r000000/conf.d/standalone-www.conf"
DICT="$PREFIX/conf/releases/r000000/conf.d/standalone-ext-dicts.conf"
PIDF=/run/syedge/syedge.pid
# agent 状态目录：与 internal/layout/layout.go 的 AgentState 一致（历史根目录 /opt/syedge 用 /var/lib/syedge-agent，其他为 <前缀>/agent）
if [[ "$PREFIX" == /opt/syedge ]]; then
  LOG_DIR=/var/log/syedge; CACHE_DIR=/var/cache/syedge; STATE_DIR=/var/lib/syedge-agent
else
  LOG_DIR="$PREFIX/logs"; CACHE_DIR="$PREFIX/cache"; STATE_DIR="$PREFIX/agent"
fi
NODE_JSON="$STATE_DIR/node.json"
PARENT=$(dirname "$PREFIX")

for p in "$PREFIX" /etc/syedge/prefix /usr/sbin/syedge-instance "$NODE_JSON"; do
  [[ ! -e "$p" && ! -L "$p" ]] || { echo "需要干净的 CI 主机（已存在 $p）" >&2; exit 1; }
done
W=$(mktemp -d); chmod 0755 "$W"
parent_made=0; [[ -d "$PARENT" ]] || parent_made=1
cleanup() {
  if [[ -n "${MASTER:-}" ]]; then sudo kill -QUIT "$MASTER" 2>/dev/null; sleep 2; fi
  sudo rm -f -- "$NODE_JSON" "$SITE"
  sudo rm -rf -- "$PREFIX"
  [[ $parent_made -eq 0 ]] || sudo rmdir -- "$PARENT" 2>/dev/null
  rm -rf "$W"
}
trap cleanup EXIT

fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -z "${GITHUB_ACTIONS:-}" ]] || echo "::error title=lua-standalone ($BASE/$ARCH)::$1"; fail=$((fail+1)); }
die() { bad "$1"; echo "---- $fail failed"; exit 1; }
syedge_t() { sudo "$SY" -p "$PREFIX" -c "$CONF" -e stderr -t -q 2>&1; }
reload() { sudo kill -HUP "$MASTER"; }
# 轮询直到响应体等于期望值（reload 后新 worker 接管需要片刻）
body_is() {
  local i b=""
  for i in 1 2 3 4 5 6 7 8 9 10; do
    b=$(curl -s --max-time 3 "http://127.0.0.1:18090$1")
    [[ "$b" == "$2" ]] && return 0
    sleep 1
  done
  return 1
}

basepkg=$(ls "$DIST"/base/syedge-*-"$ARCH".tar.gz | grep -v gmssl | head -1)
[[ $BASE == gmssl ]] && basepkg=$(ls "$DIST"/base/syedge-*-gmssl-"$ARCH".tar.gz | head -1)
luapkg=$(ls "$DIST"/syedge-lua-*-"$ARCH".tar.gz | head -1)
[[ -f "$basepkg" && -f "$luapkg" ]] || die "missing packages in $DIST"
echo "== 基础包 $(basename "$basepkg")，附加包 $(basename "$luapkg")，前缀 $PREFIX"

echo "== 安装基础包到编译期前缀并写入初始版本 r000000"
sudo tar -C / -xzf "$basepkg" || die "基础包解包失败"
[[ -x "$SY" ]] || die "基础包中没有 $SY"
GOFLAGS=-mod=vendor go run -ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/layout.defaultSystemRoot=$PREFIX" \
  ./cmd/syedge-agent package-defaults -out "$W/defaults" || die "无法生成初始版本"
[[ -f "$W/defaults/share/defaults/baseline/syedge.conf" ]] || die "初始版本缺少主配置"
getent group syedge >/dev/null || sudo groupadd -r syedge
id syedge >/dev/null 2>&1 || sudo useradd -r -g syedge -s /usr/sbin/nologin syedge
sudo mkdir -p -- "$PREFIX/conf/releases" "$LOG_DIR" "$CACHE_DIR" /run/syedge "$PREFIX/modules"
sudo cp -r -- "$W/defaults/share/defaults/baseline" "$PREFIX/conf/releases/r000000" || die "无法写入 r000000"
sudo mkdir -p -- "$PREFIX/conf/releases/r000000/conf.d"
sudo ln -sfn releases/r000000 "$PREFIX/conf/current"
# 与 agent 写出的版本一致：root 所有，数据面用户所在组可读
sudo chown -R root:syedge "$PREFIX/conf" && sudo chmod -R g+rX "$PREFIX/conf"
sudo chown -R syedge:syedge "$LOG_DIR" "$CACHE_DIR" /run/syedge
mkdir "$W/lua" && tar -C "$W/lua" -xzf "$luapkg" || die "附加包解包失败"

echo "== 启动数据面（基线配置，以 syedge 运行，之后全程不重启）"
MASTER=""
sudo setpriv --reuid=syedge --regid=syedge --init-groups --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- \
  "$SY" -p "$PREFIX" -e "$LOG_DIR/error.log" -c "$CONF" || die "syedge 无法启动：$(sudo tail -n 5 "$LOG_DIR/error.log" 2>/dev/null | tr '\n' ' ')"
for _ in $(seq 50); do sudo test -s "$PIDF" && break; sleep 0.2; done
MASTER=$(sudo cat "$PIDF" 2>/dev/null) || die "找不到 master pid"
[[ "$MASTER" =~ ^[0-9]+$ ]] || die "master pid 无效"
echo "master pid $MASTER"

# 断言 9 的基准：开始时保存主配置
sudo cp -- "$MAIN" "$W/main.orig" || die "无法保存主配置副本"
cat > "$W/site-lua.conf" <<'EOF'
server {
    listen       127.0.0.1:18090;
    server_name  standalone-lua.test;
    location = /lua {
        content_by_lua_block { ngx.say("standalone-lua-ok") }
    }
    location = /hello {
        content_by_lua_block { ngx.say(require("hello").text) }
    }
    location = /ext-require {
        content_by_lua_block {
            local mods = { "resty.redis", "resty.mysql", "resty.memcached", "resty.dns.resolver", "resty.string",
                "resty.md5", "resty.sha1", "resty.sha256", "resty.aes", "resty.random", "resty.upload",
                "resty.websocket.server", "resty.websocket.client", "resty.lock", "resty.limit.req",
                "resty.limit.conn", "resty.limit.count", "resty.limit.traffic", "tablepool" }
            local bad = {}
            for _, m in ipairs(mods) do
                local ok, err = pcall(require, m)
                if not ok then bad[#bad + 1] = m .. ": " .. tostring(err) end
            end
            if #bad == 0 then ngx.say("ext-modules-ok") else ngx.say(table.concat(bad, "; ")) end
        }
    }
    location = /ext-digest {
        content_by_lua_block {
            local str = require "resty.string"
            local sha256, md5, sha1 = require "resty.sha256", require "resty.md5", require "resty.sha1"
            local s256 = sha256:new(); s256:update("abc")
            local m5 = md5:new(); m5:update("abc")
            local s1 = sha1:new(); s1:update("abc")
            local a, b, c = str.to_hex(s256:final()), str.to_hex(m5:final()), str.to_hex(s1:final())
            if a == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
                and b == "900150983cd24fb0d6963f7d28e17f72"
                and c == "a9993e364706816aba3e25717850c26c9cd0d89d" then
                ngx.say("ext-digest-ok")
            else
                ngx.say(a, " ", b, " ", c)
            end
        }
    }
    location = /ext-aes {
        content_by_lua_block {
            local aes = require "resty.aes"
            local a = aes:new("0123456789abcdef", nil, aes.cipher(128, "cbc"), aes.hash.sha256)
            local enc = a:encrypt("hello syedge")
            local dec = enc and a:decrypt(enc)
            ngx.say(dec == "hello syedge" and "ext-aes-ok" or "ext-aes-bad")
        }
    }
    location = /ext-random {
        content_by_lua_block {
            local s = require("resty.random").bytes(16)
            ngx.say(s and #s == 16 and "ext-random-ok" or "ext-random-bad")
        }
    }
    location = /ext-lock {
        content_by_lua_block {
            local lock = require "resty.lock"
            local l = lock:new("standalone_test_locks")
            local el, err = l:lock("k1")
            local un = el and l:unlock()
            ngx.say((el and un == 1) and "ext-lock-ok" or ("ext-lock-bad " .. tostring(err)))
        }
    }
    location = /ext-limit {
        content_by_lua_block {
            local req = require "resty.limit.req"
            local lim, err = req.new("standalone_test_limits", 10, 5)
            local delay = lim and lim:incoming("ip", true)
            ngx.say((lim and delay ~= nil) and "ext-limit-ok" or ("ext-limit-bad " .. tostring(err)))
        }
    }
    location = /ext-tablepool {
        content_by_lua_block {
            local tp = require "tablepool"
            local t = tp.fetch("standalone_ext", 0, 4)
            local is_tbl = type(t) == "table"
            tp.release("standalone_ext", t)
            ngx.say(is_tbl and "ext-tablepool-ok" or "ext-tablepool-bad")
        }
    }
    location = /ext-cjson {
        content_by_lua_block {
            local cjson = require "cjson"
            local safe = require "cjson.safe"
            local t = cjson.decode(cjson.encode({ a = 1, b = { "x", "y" } }))
            local bad, err = safe.decode("{")
            ngx.say((t.a == 1 and t.b[2] == "y" and bad == nil and err ~= nil) and "ext-cjson-ok" or "ext-cjson-bad")
        }
    }
    location = /ext-redis {
        content_by_lua_block {
            local parser = require "redis.parser"
            local q = parser.build_query({ "get", "a" })
            local res, typ = parser.parse_reply("+OK\r\n")
            local ok = q == "*2\r\n$3\r\nget\r\n$1\r\na\r\n" and res == "OK" and typ == parser.STATUS_REPLY
            ngx.say(ok and "ext-redis-ok" or "ext-redis-bad")
        }
    }
    location = /ext-rds {
        content_by_lua_block {
            local parser = require "rds.parser"
            ngx.say((type(parser.parse) == "function" and parser.null ~= nil) and "ext-rds-ok" or "ext-rds-bad")
        }
    }
    location = /ext-signal {
        content_by_lua_block {
            local sig = require "resty.signal"
            local ok, err = sig.kill(ngx.worker.pid(), 0)
            ngx.say((sig.signum("HUP") == 1 and ok == true) and "ext-signal-ok" or ("ext-signal-bad " .. tostring(err)))
        }
    }
    location = /ext-shell {
        content_by_lua_block {
            local shell = require "resty.shell"
            local ok, stdout, stderr, reason = shell.run({ "echo", "hi" })
            ngx.say((ok and stdout == "hi\n") and "ext-shell-ok" or ("ext-shell-bad " .. tostring(stderr) .. " " .. tostring(reason)))
        }
    }
    location = /ext-upstream {
        content_by_lua_block {
            local names = require("ngx.upstream").get_upstreams()
            local found = false
            for _, n in ipairs(names) do
                if n == "standalone_test_up" then found = true end
            end
            ngx.say(found and "ext-upstream-ok" or "ext-upstream-bad")
        }
    }
    location = /ext-hc {
        content_by_lua_block {
            local hc = require "resty.upstream.healthcheck"
            local ok, err = hc.spawn_checker({
                shm = "standalone_hc", upstream = "standalone_test_up", type = "http",
                http_req = "GET / HTTP/1.0\r\nHost: standalone-lua.test\r\n\r\n",
                interval = 60000, timeout = 1000, fall = 2, rise = 2,
                valid_statuses = { 200 }, concurrency = 1,
            })
            ngx.say(ok and "ext-hc-ok" or ("ext-hc-bad " .. tostring(err)))
        }
    }
}
EOF
printf 'return { text = "standalone-lualib-ok" }\n' > "$W/hello.lua"
printf 'lua_shared_dict standalone_test_locks 1m;\nlua_shared_dict standalone_test_limits 1m;\nlua_shared_dict standalone_hc 1m;\nupstream standalone_test_up {\n    server 127.0.0.1:18091;\n}\n' > "$W/ext-dicts.conf"

# 1. 不带新参数安装附加包后，站点中的 Lua 指令应当失败
if sudo bash "$W/lua/install.sh" > "$W/i1.log" 2>&1; then
  sudo install -m 0644 "$W/site-lua.conf" "$SITE" || die "无法写入站点配置"
  out=$(syedge_t); rc=$?
  if [[ $rc -ne 0 && "$out" == *content_by_lua_block* ]]; then
    ok "1 不带新参数安装附加包后，站点中的 content_by_lua_block 校验失败（问题存在）"
  else
    bad "1 站点中的 Lua 指令应当被拒绝，实际 rc=$rc：${out:0:200}"
  fi
else
  bad "1 不带新参数安装附加包失败：$(tail -n 3 "$W/i1.log" | tr '\n' ' ')"
fi

# 2. 独立模式启用
stat_before=$(sudo stat -c '%U:%G %a' "$MAIN")
sudo bash "$W/lua/install.sh" --standalone > "$W/i2.log" 2>&1; rc2=$?
if [[ $rc2 -eq 0 ]] \
  && sudo sed -n 1p "$MAIN" | grep -Eq '^load_module  .+/modules/lua/ndk_http_module\.so;$' \
  && sudo sed -n 2p "$MAIN" | grep -Eq '^load_module  .+/modules/lua/ngx_http_lua_module\.so;$' \
  && sudo test -f "$NEWCONF"; then
  ok "2 --standalone 成功：主配置前两行为 load_module，conf.d/00-lua-standalone.conf 已生成"
else
  bad "2 --standalone 失败（rc=$rc2）：$(tail -n 3 "$W/i2.log" | tr '\n' ' ')"
fi
stat_after=$(sudo stat -c '%U:%G %a' "$MAIN")
[[ "$stat_before" == "$stat_after" ]] && ok "2 主配置属主与权限不变（$stat_after）" || bad "2 主配置属主或权限改变：$stat_before -> $stat_after"

# 3. 站点配置校验通过，reload 后 Lua 输出生效
# （共享内存字典 lua_shared_dict 只能在 http 级声明，放在单独的 conf.d 文件中，在启用之后、reload 之前安装）
sudo install -m 0644 "$W/ext-dicts.conf" "$DICT" || die "无法写入共享字典配置"
out=$(syedge_t); rc=$?
reload
if [[ $rc -eq 0 ]] && body_is /lua "standalone-lua-ok"; then
  ok "3 站点 Lua 配置 syedge -t 通过，reload 后响应为 Lua 输出"
else
  bad "3 站点 Lua 配置未生效（rc=$rc）：${out:0:200}"
fi

# 4. 附加包的 lualib 目录中的模块可被 require
if sudo install -m 0644 "$W/hello.lua" "$PREFIX/lualib/hello.lua" && body_is /hello "standalone-lualib-ok"; then
  ok "4 <根目录>/lualib/hello.lua 可被站点 require"
else
  bad "4 <根目录>/lualib 中的模块未被加载"
fi

# 5. 验收用的四层阶段片段通过校验（只校验，不请求）
sudo install -m 0644 "$HERE/standalone-lua-response-filter.conf" "$PREFIX/conf/current/conf.d/standalone-lua-response-filter.conf"
out=$(syedge_t); rc=$?
sudo rm -f -- "$PREFIX/conf/current/conf.d/standalone-lua-response-filter.conf"
[[ $rc -eq 0 ]] && ok "5 response-filter 片段 syedge -t 通过" || bad "5 response-filter 片段校验失败：${out:0:200}"

# 13-20. 启用期间附加包随带的 Lua 库（extlib）可用：00 文件的 lua_package_path 为三段（lualib、extlib、<根目录>/lualib）
extok() { # <编号> <说明> <路径> <期望响应体>
  if body_is "$3" "$4"; then ok "$1 $2"; else bad "$1 $2（响应：$(curl -s --max-time 3 "http://127.0.0.1:18090$3" | head -c 200)）"; fi
}
line13=$(sudo grep '^lua_package_path' "$NEWCONF")
want13="lua_package_path   \"$PREFIX/modules/lua/lualib/?.lua;$PREFIX/modules/lua/extlib/?.lua;$PREFIX/lualib/?.lua\";"
if [[ "$line13" == "$want13" ]]; then ok "13 00 文件的 lua_package_path 为三段：lualib、extlib、<根目录>/lualib"; else bad "13 lua_package_path 不是预期的三段：$line13"; fi
extok 14 "resty.* 与 tablepool 共 19 个模块全部可 require" /ext-require "ext-modules-ok"
extok 15 "resty.sha256 / md5 / sha1 对固定输入的摘要正确（resty.string.to_hex）" /ext-digest "ext-digest-ok"
extok 16 "resty.aes 加解密往返" /ext-aes "ext-aes-ok"
extok 17 "resty.random.bytes 返回指定长度" /ext-random "ext-random-ok"
extok 18 "resty.lock 加锁与解锁" /ext-lock "ext-lock-ok"
extok 19 "resty.limit.req 可创建对象并计数" /ext-limit "ext-limit-ok"
extok 20 "tablepool fetch / release" /ext-tablepool "ext-tablepool-ok"

# 21-28. D1 第二阶段的编译库与上游模块（只在独立模式可用）：00 文件的 lua_package_cpath 指向 extlib，
# 主配置第三行加载 lua-upstream 模块，C 库、resty.signal、resty.shell 与 resty.upstream.healthcheck 在真实数据面上可用
line21=$(sudo grep '^lua_package_cpath' "$NEWCONF")
want21="lua_package_cpath  \"$PREFIX/modules/lua/extlib/?.so\";"
if [[ "$line21" == "$want21" ]]; then ok "21 00 文件的 lua_package_cpath 为 extlib/?.so"; else bad "21 lua_package_cpath 不是预期值：$line21"; fi
extok 22 "cjson.encode / decode 往返，cjson.safe 对坏输入返回 nil 与错误" /ext-cjson "ext-cjson-ok"
extok 23 "redis.parser 的 build_query 与 parse_reply" /ext-redis "ext-redis-ok"
extok 24 "rds.parser 可 require" /ext-rds "ext-rds-ok"
extok 25 "resty.signal 的 signum(HUP) 与对本 worker 的 kill(pid, 0)" /ext-signal "ext-signal-ok"
extok 26 "resty.shell 运行 echo hi 并返回标准输出 hi" /ext-shell "ext-shell-ok"
if sudo sed -n 3p "$MAIN" | grep -Eq '^load_module  .+/modules/lua/ngx_http_lua_upstream_module\.so;$'; then
  extok 27 "lua-upstream 模块已加载，ngx.upstream 列出站点定义的 upstream" /ext-upstream "ext-upstream-ok"
else
  bad "27 主配置第三行应为 lua-upstream 模块的 load_module"
fi
extok 28 "resty.upstream.healthcheck 对该 upstream 的 spawn_checker 成功（共享字典 standalone_hc）" /ext-hc "ext-hc-ok"

# 6. 重复启用报错，文件不变
sudo cp -- "$MAIN" "$W/main.before6" && sudo cp -- "$NEWCONF" "$W/new.before6"
sudo bash "$W/lua/install.sh" --standalone > "$W/i6.log" 2>&1; rc6=$?
if [[ $rc6 -ne 0 ]] && sudo cmp -s "$MAIN" "$W/main.before6" && sudo cmp -s "$NEWCONF" "$W/new.before6"; then
  ok "6 重复执行 --standalone 报错退出，文件不变"
else
  bad "6 重复执行 --standalone 应当报错且不改动文件（rc=$rc6）"
fi

# 7. 卸载被拒绝并提示 --standalone-disable
out=$(sudo bash "$W/lua/uninstall.sh" 2>&1); rc=$?
if [[ $rc -ne 0 && "$out" == *--standalone-disable* ]]; then
  ok "7 uninstall.sh 拒绝卸载并提示 --standalone-disable"
else
  bad "7 uninstall.sh 应当拒绝并提示 --standalone-disable（rc=$rc）：${out:0:200}"
fi

# 8. 站点仍在用 Lua 时停用：报错退出，配置恢复原状，syedge -t 仍通过
sudo cp -- "$MAIN" "$W/main.before8" && sudo cp -- "$NEWCONF" "$W/new.before8"
sudo bash "$W/lua/install.sh" --standalone-disable > "$W/i8.log" 2>&1; rc8=$?
if [[ $rc8 -ne 0 ]]; then ok "8 站点仍在用 Lua 时 --standalone-disable 报错退出"; else bad "8 站点仍在用 Lua 时停用应当报错"; fi
if sudo cmp -s "$MAIN" "$W/main.before8" && sudo cmp -s "$NEWCONF" "$W/new.before8"; then
  ok "8 停用失败后主配置与 conf.d/00-lua-standalone.conf 恢复原状"
else
  bad "8 停用失败后配置未恢复原状"
fi
out=$(syedge_t); rc=$?
[[ $rc -eq 0 ]] && ok "8 恢复后 syedge -t 仍通过" || bad "8 恢复后 syedge -t 失败：${out:0:200}"

# 9. 去掉 Lua 站点后停用并卸载：都成功，主配置与最初逐字节相同
sudo rm -f -- "$SITE" "$DICT"
sudo bash "$W/lua/install.sh" --standalone-disable > "$W/i9.log" 2>&1; rc9=$?
reload; sleep 2
sudo bash "$W/lua/uninstall.sh" > "$W/u9.log" 2>&1; rc9u=$?
if [[ $rc9 -eq 0 && $rc9u -eq 0 ]] && sudo cmp -s "$MAIN" "$W/main.orig" && ! sudo test -e "$NEWCONF" && ! sudo test -e "$PREFIX/modules/lua"; then
  ok "9 停用与卸载成功，主配置与最初逐字节相同"
else
  bad "9 停用或卸载失败（停用 rc=$rc9，卸载 rc=$rc9u）：$(tail -n 2 "$W/i9.log" "$W/u9.log" | tr '\n' ' ')"
fi

# 10. conf/current 指向非初始版本时拒绝启用，不改动文件
sudo cp -a -- "$PREFIX/conf/releases/r000000" "$PREFIX/conf/releases/r000001"
sudo ln -sfn releases/r000001 "$PREFIX/conf/current"
sudo cp -- "$PREFIX/conf/releases/r000001/syedge.conf" "$W/main10.before"
sudo bash "$W/lua/install.sh" --standalone > "$W/i10.log" 2>&1; rc10=$?
if [[ $rc10 -ne 0 ]] && ! sudo test -e "$PREFIX/conf/releases/r000001/conf.d/00-lua-standalone.conf" \
  && sudo cmp -s "$PREFIX/conf/releases/r000001/syedge.conf" "$W/main10.before"; then
  ok "10 配置版本不是 r000000 时 --standalone 报错退出，文件不变"
else
  bad "10 配置版本不是 r000000 时应当报错且不改动文件（rc=$rc10）"
fi
sudo ln -sfn releases/r000000 "$PREFIX/conf/current"
sudo rm -rf -- "$PREFIX/conf/releases/r000001"

# 11. agent 状态目录存在 node.json 时拒绝启用，不改动文件
sudo mkdir -p -- "$STATE_DIR" && sudo touch -- "$NODE_JSON"
sudo bash "$W/lua/install.sh" --standalone > "$W/i11.log" 2>&1; rc11=$?
if [[ $rc11 -ne 0 ]] && sudo cmp -s "$MAIN" "$W/main.orig" && ! sudo test -e "$NEWCONF"; then
  ok "11 已注册节点（node.json 存在）时 --standalone 报错退出，文件不变"
else
  bad "11 已注册节点时应当报错且不改动文件（rc=$rc11）"
fi
sudo rm -f -- "$NODE_JSON"

# 12. 整个过程中 master 的 pid 不变，且仍在运行
now=$(sudo cat "$PIDF" 2>/dev/null)
if [[ "$now" == "$MASTER" ]] && sudo kill -0 "$MASTER" 2>/dev/null; then
  ok "12 整个过程 master pid 不变（$MASTER）"
else
  bad "12 master pid 改变或已退出：$MASTER -> ${now:-无}"
fi

echo "---- $fail failed"
[[ $fail -eq 0 ]] || exit 1
echo "Lua $BASE 独立模式一条命令启用与停用：OK"
