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
}
EOF
printf 'return { text = "standalone-lualib-ok" }\n' > "$W/hello.lua"

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
sudo rm -f -- "$SITE"
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
