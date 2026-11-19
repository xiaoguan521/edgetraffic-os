#!/usr/bin/env bash
# 验证“不写 lua_package_path 也能 require 本前缀下的库”（LuaJIT 默认搜索路径在编译期指向安装前缀）。
#   用法：scripts/ci/lua-default-path.sh <dist-lua 目录> <standard|gmssl>
#   环境准备照 lua-standalone.sh：基础包装到编译期前缀，master 以 syedge 运行，附加包用发布包里的 install.sh（不带 --standalone）。
#   主配置自己写：load_module 两行 + 一个 server，全程不出现 lua_package_path（D4 除外）。
#   断言：
#     D1 配置校验与启动时没有 “failed to load the 'resty.core' module”（附加包自带库按默认路径找到）
#     D2 <前缀>/lualib 下运维自己的库 require 成功，不需要改配置
#     D3 <前缀>/modules/lua/extlib 下的编译库（cjson）按默认 cpath 找到
#     D4 写了带 ";;" 的 lua_package_path 时，原默认路径仍然有效（追加而不是覆盖）
#     D5 整个过程不重启：换配置只发 HUP，master pid 不变
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
PIDF="$PREFIX/run/syedge.pid"
# agent 状态目录：与 internal/layout/layout.go 的 AgentState 一致（<前缀>/agent）
LOG_DIR="$PREFIX/logs"; CACHE_DIR="$PREFIX/cache"; STATE_DIR="$PREFIX/agent"; RUN_DIR="$PREFIX/run"
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
sudo mkdir -p -- "$PREFIX/conf/releases" "$LOG_DIR" "$CACHE_DIR" "$RUN_DIR" "$PREFIX/modules"
sudo cp -r -- "$W/defaults/share/defaults/baseline" "$PREFIX/conf/releases/r000000" || die "无法写入 r000000"
sudo mkdir -p -- "$PREFIX/conf/releases/r000000/conf.d"
sudo ln -sfn releases/r000000 "$PREFIX/conf/current"
# 与 agent 写出的版本一致：root 所有，数据面用户所在组可读
sudo chown -R root:syedge "$PREFIX/conf" && sudo chmod -R g+rX "$PREFIX/conf"
sudo chown -R syedge:syedge "$LOG_DIR" "$CACHE_DIR" "$RUN_DIR"
mkdir "$W/lua" && tar -C "$W/lua" -xzf "$luapkg" || die "附加包解包失败"

echo "== 启动数据面（基线配置，以 syedge 运行，之后全程不重启）"
MASTER=""
sudo setpriv --reuid=syedge --regid=syedge --init-groups --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- \
  "$SY" -p "$PREFIX" -e "$LOG_DIR/error.log" -c "$CONF" || die "syedge 无法启动：$(sudo tail -n 5 "$LOG_DIR/error.log" 2>/dev/null | tr '\n' ' ')"
for _ in $(seq 50); do sudo test -s "$PIDF" && break; sleep 0.2; done
MASTER=$(sudo cat "$PIDF" 2>/dev/null) || die "找不到 master pid"
[[ "$MASTER" =~ ^[0-9]+$ ]] || die "master pid 无效"
echo "master pid $MASTER"

CONF_MAIN="$W/default-path.conf"
write_conf() { # <额外的 http 级指令>
  cat > "$CONF_MAIN" <<CONFEOF
load_module  $PREFIX/modules/lua/ndk_http_module.so;
load_module  $PREFIX/modules/lua/ngx_http_lua_module.so;
worker_processes 1;
pid $PIDF;
events { worker_connections 256; }
http {
    access_log off;
    # 自己写的主配置没有基线里的临时目录设置，显式写出便于独立模式的排查
    client_body_temp_path $CACHE_DIR/client_temp;
    proxy_temp_path       $CACHE_DIR/proxy_temp;
    fastcgi_temp_path     $CACHE_DIR/fastcgi_temp;
    uwsgi_temp_path       $CACHE_DIR/uwsgi_temp;
    scgi_temp_path        $CACHE_DIR/scgi_temp;
    $1
    server {
        listen       127.0.0.1:18090;
        server_name  default-path.test;
        location = /core { content_by_lua_block { ngx.say(require("resty.core") and "core-ok" or "core-bad") } }
        location = /own  { content_by_lua_block { ngx.say(require("hello_default").text) } }
        location = /cjson {
            content_by_lua_block {
                local cjson = require "cjson"
                ngx.say(cjson.decode(cjson.encode({ a = 1 })).a == 1 and "cjson-ok" or "cjson-bad")
            }
        }
        location = /extra { content_by_lua_block { ngx.say(require("only_extra").text) } }
    }
}
CONFEOF
}
# 轮询直到响应体等于期望值
body_is() {
  local i b=""
  for i in 1 2 3 4 5 6 7 8 9 10; do
    b=$(curl -s --max-time 3 "http://127.0.0.1:18090$1")
    [[ "$b" == "$2" ]] && return 0
    sleep 1
  done
  echo "     实际：${b:0:160}"
  return 1
}

# 安装附加包（不带 --standalone），并准备运维自己的库
sudo bash "$W/lua/install.sh" > "$W/i1.log" 2>&1 || die "附加包安装失败：$(tail -n 3 "$W/i1.log" | tr '\n' ' ')"
sudo mkdir -p -- "$PREFIX/lualib"
printf 'return { text = "default-path-own-ok" }\n' | sudo tee "$PREFIX/lualib/hello_default.lua" >/dev/null
sudo chmod 0644 "$PREFIX/lualib/hello_default.lua"
mkdir -p "$W/extra"; printf 'return { text = "default-path-extra-ok" }\n' > "$W/extra/only_extra.lua"; chmod -R a+rX "$W/extra"

# 先停掉基线 master，换成只带 load_module 与本测试 server 的主配置
sudo kill -QUIT "$MASTER"; sleep 2; MASTER=""
write_conf ""
out=$(sudo "$SY" -p "$PREFIX" -c "$CONF_MAIN" -e stderr -t 2>&1); rc=$?
if [[ $rc -eq 0 && "$out" != *"failed to load the 'resty.core'"* ]]; then
  ok "D1 没有 lua_package_path 的配置 -t 通过，且没有 resty.core 告警"
else
  bad "D1 -t rc=$rc：${out:0:300}"
fi
# 没有 master 时用 root 跑 -t 会留下 root 所有的 pid 文件，之后以 syedge 启动会写不了它
sudo rm -f -- "$PIDF"
sudo setpriv --reuid=syedge --regid=syedge --init-groups --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- \
  "$SY" -p "$PREFIX" -e "$LOG_DIR/error.log" -c "$CONF_MAIN" || die "syedge 无法启动：$(sudo tail -n 5 "$LOG_DIR/error.log" 2>/dev/null | tr '\n' ' ')"
for _ in $(seq 50); do sudo test -s "$PIDF" && break; sleep 0.2; done
MASTER=$(sudo cat "$PIDF" 2>/dev/null) || die "找不到 master pid"
body_is /core core-ok && ok "D1 resty.core 按默认路径加载" || bad "D1 /core 未返回 core-ok"
if sudo grep -q "failed to load the 'resty.core'" "$LOG_DIR/error.log" 2>/dev/null; then bad "D1 error.log 里有 resty.core 告警"; else ok "D1 error.log 没有 resty.core 告警"; fi
body_is /own default-path-own-ok && ok "D2 <前缀>/lualib 下的自有库 require 成功" || bad "D2 自有库没有被默认路径找到"
body_is /cjson cjson-ok && ok "D3 extlib 下的 cjson 按默认 cpath 找到" || bad "D3 cjson 没有被默认 cpath 找到"

# D4 带 ";;" 的写法：追加自己的目录，原默认路径仍然有效
write_conf "lua_package_path \"$W/extra/?.lua;;\";"
out=$(sudo "$SY" -p "$PREFIX" -c "$CONF_MAIN" -e stderr -t 2>&1) || bad "D4 -t 未通过：${out:0:200}"
sudo kill -HUP "$MASTER"
body_is /extra default-path-extra-ok && ok "D4 lua_package_path 带 ;; 时自己的目录生效" || bad "D4 追加的目录没有生效"
body_is /core core-ok && body_is /own default-path-own-ok && ok "D4 带 ;; 时原默认路径仍然有效（追加而不是覆盖）" || bad "D4 带 ;; 后默认路径失效"

now=$(sudo cat "$PIDF" 2>/dev/null)
if [[ "$now" == "$MASTER" ]] && sudo kill -0 "$MASTER" 2>/dev/null; then ok "D5 全程只发 HUP，master pid 不变（$MASTER）"; else bad "D5 master pid 改变或已退出"; fi
sudo rm -f -- "$PREFIX/lualib/hello_default.lua"

echo "---- $fail failed"
[[ $fail -eq 0 ]] || exit 1
echo "Lua $BASE 默认搜索路径：OK"
