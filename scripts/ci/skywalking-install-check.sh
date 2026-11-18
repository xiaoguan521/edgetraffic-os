#!/usr/bin/env bash
# 校验 install.sh 里 SkyWalking 步骤的失败恢复（deploy/node/skywalking-install.sh），不需要 root、systemd 或真实数据面：
#   - 冲突检查必须能在启用 Lua 之前单独运行，且失败时不写任何文件；
#   - 写入后配置校验失败，必须还原用户原有的同名 10-skywalking.conf，并撤销本次新增的库文件；
#   - install.sh 必须在安装 Lua 附加包之前调用冲突检查。
set -uo pipefail
cd "$(dirname "$0")/../.."
LIB=deploy/node/skywalking-install.sh
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -z "${GITHUB_ACTIONS:-}" ]] || echo "::error title=skywalking-install-check::$1"; fail=$((fail+1)); }
[[ -f "$LIB" ]] || { bad "缺少 $LIB"; echo "---- $fail failed"; exit 1; }
# shellcheck source=/dev/null
source "$LIB"

W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
# 每个用例一套全新的 R（安装根）与 SWSRC（包内库）
fresh() {
  R="$W/r$1"; SWSRC="$W/pkg$1/lualib"; STAGE="$W/stage$1"
  mkdir -p "$R/sbin" "$R/lualib" "$R/conf/current/conf.d" "$SWSRC/skywalking" "$STAGE"
  printf 'new tracer\n' > "$SWSRC/skywalking/tracer.lua"
  printf 'new prom\n' > "$SWSRC/prometheus.lua"
  printf 'NEW INIT\n' > "$STAGE/10-skywalking.conf"
  # 假的 syedge：-t 是否成功由 FAKE_T_FAIL 决定
  printf '#!/bin/sh\n[ "${FAKE_T_FAIL:-0}" = 1 ] && { echo "bad config" >&2; exit 1; }\nexit 0\n' > "$R/sbin/syedge"
  chmod 0755 "$R/sbin/syedge"
  CONFD="$R/conf/current/conf.d"
}
snapshot() { (cd "$R" && find lualib conf -type f -exec sh -c 'printf "%s %s\n" "$1" "$(cksum < "$1")"' _ {} \; | sort); }

# 1. 同名但内容不同的库文件：冲突检查失败，且什么都没写
fresh 1; printf 'user lib\n' > "$R/lualib/prometheus.lua"; before=$(snapshot)
if sw_precheck "$R" "$SWSRC" "$CONFD" 2>"$W/e1"; then bad "已有不同内容的同名库文件没有被拒绝"; else
  grep -q 'prometheus.lua' "$W/e1" && [[ "$(snapshot)" == "$before" ]] && ok "库文件冲突：拒绝且不写文件" || bad "库文件冲突：未指出文件或改动了磁盘"; fi

# 2. conf.d 已有其他文件定义 init_worker_by_lua_block
fresh 2; printf 'init_worker_by_lua_block { }\n' > "$CONFD/20-mine.conf"
if sw_precheck "$R" "$SWSRC" "$CONFD" 2>"$W/e2"; then bad "重复的 init_worker_by_lua_block 没有被拒绝"; else
  grep -q '20-mine.conf' "$W/e2" && ok "重复初始化：拒绝并指出文件" || bad "重复初始化：未指出文件"; fi

# 3. 内容相同的已有库文件与已有的 10-skywalking.conf 不算冲突
fresh 3; cp "$SWSRC/prometheus.lua" "$R/lualib/prometheus.lua"; printf 'init_worker_by_lua_block { }\n' > "$CONFD/10-skywalking.conf"
sw_precheck "$R" "$SWSRC" "$CONFD" 2>/dev/null && ok "相同库文件与自身的旧 10-skywalking.conf 不算冲突" || bad "不应拒绝相同库文件或旧的 10-skywalking.conf"

# 4. 校验失败：还原用户原有的 10-skywalking.conf，撤销新增库文件，保留原有库文件
fresh 4; printf 'USER INIT\n' > "$CONFD/10-skywalking.conf"; cp "$SWSRC/prometheus.lua" "$R/lualib/prometheus.lua"; before=$(snapshot)
if FAKE_T_FAIL=1 sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null; then bad "校验失败时 sw_apply 应返回非 0"; else
  [[ "$(snapshot)" == "$before" ]] && ok "校验失败：原有 10-skywalking.conf 与库文件原样还原，新增库文件已撤销" || { bad "校验失败后磁盘与安装前不一致"; diff <(echo "$before") <(snapshot); }; fi

# 5. 校验失败且原先没有 10-skywalking.conf：删除新文件
fresh 5; before=$(snapshot)
FAKE_T_FAIL=1 sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null
[[ "$(snapshot)" == "$before" ]] && ok "校验失败（全新安装）：撤销全部改动" || bad "校验失败（全新安装）后仍有残留"

# 6. 成功路径
fresh 6
if sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null \
   && [[ "$(cat "$CONFD/10-skywalking.conf")" == "NEW INIT" && -f "$R/lualib/skywalking/tracer.lua" && -f "$R/lualib/prometheus.lua" ]]; then ok "成功路径：库与初始化文件已写入"; else bad "成功路径没有写入全部文件"; fi

# 7. sw_apply 自己也要拒绝冲突（防止预检之后出现新冲突）
fresh 7; printf 'user lib\n' > "$R/lualib/prometheus.lua"; before=$(snapshot)
sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null && bad "sw_apply 没有拒绝冲突" || { [[ "$(snapshot)" == "$before" ]] && ok "sw_apply 遇到冲突时拒绝且不写文件" || bad "sw_apply 冲突时改动了磁盘"; }

# 8. install.sh 的顺序：预检在 Lua 附加包安装之前
pre=$(grep -n 'sw_precheck' deploy/node/install.sh | head -1 | cut -d: -f1); lua=$(grep -n 'echo "== 安装 Lua 附加包"' deploy/node/install.sh | head -1 | cut -d: -f1)
[[ -n "$pre" && -n "$lua" && "$pre" -lt "$lua" ]] && ok "install.sh 在安装 Lua 附加包之前运行冲突检查" || bad "install.sh 的冲突检查没有排在 Lua 附加包安装之前（pre=$pre lua=$lua）"

echo "---- $fail failed"
[[ $fail -eq 0 ]]
