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

# 9. 全新节点：conf.d 还不存在（由随后的 Lua 安装步骤创建），预检不能因此失败
fresh 9; rm -rf "$CONFD"; before=$(snapshot)
if sw_precheck "$R" "$SWSRC" "$CONFD" 2>"$W/e9"; then [[ "$(snapshot)" == "$before" ]] && ok "conf.d 尚不存在时预检通过且不写文件" || bad "预检改动了磁盘"; else bad "conf.d 尚不存在时预检失败：$(cat "$W/e9")"; fi
# 但真正写入时 conf.d 必须存在
if sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null; then bad "conf.d 不存在时 sw_apply 不应成功"; else ok "写入时 conf.d 不存在则拒绝"; fi

# 10/11. 写入失败（install 失败）必须返回非 0 并还原：sw_apply 在 ||、if 里被调用时 set -e 不生效，必须显式检查每一步
fresh 10; printf 'USER INIT\n' > "$CONFD/10-skywalking.conf"; before=$(snapshot)
install() { case "$*" in *10-skywalking.conf) return 1 ;; esac; command install "$@"; }
if sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null; then bad "写入 10-skywalking.conf 失败却返回成功"; else
  [[ "$(snapshot)" == "$before" ]] && ok "写入初始化文件失败：返回非 0，原文件与库文件还原" || { bad "写入初始化文件失败后磁盘与安装前不一致"; diff <(echo "$before") <(snapshot); }; fi
unset -f install
fresh 11; before=$(snapshot)
install() { case "$*" in *prometheus.lua) return 1 ;; esac; command install "$@"; }
if sw_apply "$R" "$SWSRC" "$STAGE/10-skywalking.conf" "$CONFD" "$STAGE/t.err" 2>/dev/null; then bad "写入库文件失败却返回成功"; else
  [[ "$(snapshot)" == "$before" ]] && ok "写入库文件失败：返回非 0，已写入的文件被撤销" || { bad "写入库文件失败后仍有残留"; diff <(echo "$before") <(snapshot); }; fi
unset -f install

# 8. install.sh 的顺序：预检在 Lua 附加包安装之前
pre=$(grep -n 'sw_precheck' deploy/node/install.sh | head -1 | cut -d: -f1); lua=$(grep -n 'echo "== 安装 Lua 附加包"' deploy/node/install.sh | head -1 | cut -d: -f1)
[[ -n "$pre" && -n "$lua" && "$pre" -lt "$lua" ]] && ok "install.sh 在安装 Lua 附加包之前运行冲突检查" || bad "install.sh 的冲突检查没有排在 Lua 附加包安装之前（pre=$pre lua=$lua）"

# 12. SkyWalking 写入失败时，install.sh 要先停用刚启用的 Lua 独立模式，提示里的“重新执行”才真的可行
dis=$(grep -n 'standalone-disable' deploy/node/install.sh | grep -v '^[0-9]*:#' | head -1 | cut -d: -f1); apl=$(grep -n 'sw_apply "\$R"' deploy/node/install.sh | head -1 | cut -d: -f1)
[[ -n "$dis" && -n "$apl" && "$dis" -gt "$apl" ]] && ok "sw_apply 失败后 install.sh 会停用 Lua 独立模式再退出" || bad "install.sh 没有在 sw_apply 失败后停用 Lua 独立模式（disable=$dis apply=$apl）"

# 13. 初始化文件的快照与还原：reload 失败时让磁盘回到 sw_apply 之前，已有的 10-skywalking.conf 放回原处，原先没有则删除
fresh 13; printf 'USER INIT\n' > "$CONFD/10-skywalking.conf"
sw_snapshot_conf "$CONFD" "$STAGE/pre.conf" && printf 'NEW INIT\n' > "$CONFD/10-skywalking.conf"
if sw_restore_conf "$CONFD" "$STAGE/pre.conf" && [[ "$(cat "$CONFD/10-skywalking.conf")" == "USER INIT" ]]; then ok "还原：原有的 10-skywalking.conf 放回原处"; else bad "还原没有放回原有的 10-skywalking.conf"; fi
fresh 14
sw_snapshot_conf "$CONFD" "$STAGE/pre.conf" && printf 'NEW INIT\n' > "$CONFD/10-skywalking.conf"
if sw_restore_conf "$CONFD" "$STAGE/pre.conf" && [[ ! -e "$CONFD/10-skywalking.conf" ]]; then ok "还原：原先没有 10-skywalking.conf 则删除本次写入的"; else bad "还原没有删除本次新增的 10-skywalking.conf"; fi
fresh 15; printf 'USER INIT\n' > "$CONFD/10-skywalking.conf"; sw_snapshot_conf "$CONFD" "$STAGE/pre.conf"
rm -f "$CONFD/10-skywalking.conf"; mkdir "$CONFD/10-skywalking.conf"   # 目标位置被占，还原必须报错而不是假装成功
if sw_restore_conf "$CONFD" "$STAGE/pre.conf" 2>/dev/null; then bad "还原失败却返回成功"; else ok "还原失败返回非 0"; fi

# 14. 独立模式 reload 失败：install.sh 必须先还原 SkyWalking 初始化文件，再停用 Lua 独立模式
#     （顺序不能反：停用后 init_worker_by_lua_block 没有 Lua 模块，配置校验会失败）
rl=$(grep -n 'if ! systemctl reload syedge.service' deploy/node/install.sh | head -1 | cut -d: -f1)
[[ -n "$rl" ]] && body=$(sed -n "$((rl-1)),$((rl+14))p" deploy/node/install.sh) || body=""
r1=$(grep -n 'sw_restore_conf' <<<"$body" | head -1 | cut -d: -f1); r2=$(grep -n 'standalone-disable' <<<"$body" | head -1 | cut -d: -f1)
[[ -n "$r1" && -n "$r2" && "$r1" -lt "$r2" ]] && ok "reload 失败后先还原 SkyWalking 文件，再停用 Lua 独立模式" || bad "install.sh 的 reload 失败分支没有先还原 SkyWalking 文件再停用 Lua（restore=$r1 disable=$r2）"

# 15. 记录 10-skywalking.conf 的快照必须在安装 Lua 附加包（启用独立模式）之前：之后才记录的话，快照失败只能 die，
#     Lua 独立模式已启用却没有停用，重新执行会被“主配置已含 load_module”拒绝
snap=$(grep -n 'sw_snapshot_conf "' deploy/node/install.sh | head -1 | cut -d: -f1); lua=$(grep -n 'echo "== 安装 Lua 附加包"' deploy/node/install.sh | head -1 | cut -d: -f1)
[[ -n "$snap" && -n "$lua" && "$snap" -lt "$lua" ]] && ok "install.sh 在安装 Lua 附加包之前记录 10-skywalking.conf 快照" || bad "install.sh 的快照没有排在 Lua 附加包安装之前（snapshot=$snap lua=$lua）"

echo "---- $fail failed"
[[ $fail -eq 0 ]]
