#!/usr/bin/env bash
# syapp-lua 附加包卸载（P10）：在节点上以 root 执行  bash uninstall.sh
# 当前生效的配置仍在加载附加包模块时拒绝卸载（否则下一次 reload 会失败）：
# 先在控制台去掉 JWT 规则并发布，再卸载。卸载不重启、不停止 syapp。
set -euo pipefail
ROOT="${SYAPP_ROOT:-/opt/syapp}"
M="$ROOT/modules"
die() { echo "syapp-lua: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 || -n "${SYAPP_ROOT:-}" ]] || die "请以 root 执行"
[[ -e "$M/lua" ]] || { echo "syapp-lua 未安装"; exit 0; }
if grep -qs "modules/lua/" "$ROOT/conf/current/syapp.conf"; then
  die "当前配置正在使用 Lua 规则：先在控制台去掉 JWT 规则并发布到本节点，再卸载"
fi
rm -f "$M/lua"
rm -rf "$M"/lua-* "$M"/.lua-*.tmp
echo "syapp-lua 已卸载"
