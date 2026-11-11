#!/usr/bin/env bash
# syedge-lua 附加包卸载（P10）：在节点上以 root 执行  bash uninstall.sh
# 当前生效的配置仍在加载附加包模块时拒绝卸载（否则下一次 reload 会失败）：
# 先在控制台去掉 JWT 规则并发布，再卸载。卸载不重启、不停止 syedge。
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=install.sh
source "$HERE/install.sh"
resolve_target "$@"
[[ -z "$STANDALONE_ACTION" ]] || die "--standalone 与 --standalone-disable 只适用于 install.sh"
M="$ROOT/modules"

[[ $(id -u) -eq 0 || -n "${SYEDGE_ROOT:-}" ]] || die "请以 root 执行"
[[ -e "$M/lua" ]] || { echo "syedge-lua 未安装"; exit 0; }
echo "已选择实例：$SELECTED；安装根目录：$ROOT。"
[[ "$TARGET_MODE" != shared ]] || echo "卸载会影响同一目录下的其他共享实例。"
confs=()
if [[ "$TARGET_MODE" == independent ]]; then
  confs=("$ROOT/conf/current/syedge.conf")
elif [[ "$TARGET_COUNT" -gt 1 ]]; then
  for ((i=0; i<TARGET_COUNT; i++)); do
    [[ "${TARGET_MODES[$i]}" == shared && "${TARGET_ROOTS[$i]}" == "$ROOT" ]] || continue
    if [[ "${TARGET_NAMES[$i]}" == default ]]; then confs+=("$ROOT/conf/current/syedge.conf"); else confs+=("$BASE_ROOT/instances/${TARGET_NAMES[$i]}/conf/current/syedge.conf"); fi
  done
else
  confs=("$ROOT/conf/current/syedge.conf" "$ROOT"/instances/*/conf/current/syedge.conf)
fi
for conf in "${confs[@]}"; do
  if grep -qs "modules/lua/" "$conf"; then
    if [[ -e "$(dirname "$conf")/conf.d/00-lua-standalone.conf" ]]; then
      die "独立模式 Lua 仍在使用（${conf}）：先执行 bash install.sh --standalone-disable（具名实例加 --instance <实例名>），再卸载"
    fi
    die "当前配置正在使用 Lua 规则（${conf}）：先在控制台去掉 JWT 规则并发布到本节点，再卸载"
  fi
done
rm -f "$M/lua"
rm -rf "$M"/lua-* "$M"/.lua-*.tmp
echo "syedge-lua 已卸载"
