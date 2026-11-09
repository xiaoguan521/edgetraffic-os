#!/usr/bin/env bash
# syedge-lua 附加包卸载（P10）：在节点上以 root 执行  bash uninstall.sh
# 当前生效的配置仍在加载附加包模块时拒绝卸载（否则下一次 reload 会失败）：
# 先在控制台去掉 JWT 规则并发布，再卸载。卸载不重启、不停止 syedge。
set -euo pipefail
die() { echo "syedge-lua: $*" >&2; exit 1; }
if [[ -n "${SYEDGE_ROOT:-}" ]]; then
  ROOT="$SYEDGE_ROOT"
elif [[ -x /usr/libexec/syedge/syedge-instance ]]; then
  ROOT="$(/usr/libexec/syedge/syedge-instance prefix)" || die "无法校验安装根目录"
elif [[ ! -e /etc/syedge/prefix && ! -L /etc/syedge/prefix ]]; then
  ROOT=/opt/syedge
elif [[ -x /usr/sbin/syedge-instance ]]; then
  ROOT="$(/usr/sbin/syedge-instance prefix)" || die "无法校验安装根目录"
else
  die "缺少安装根目录校验工具"
fi
[[ "$ROOT" == /* && "$ROOT" != / ]] || die "安装根目录无效"
M="$ROOT/modules"

[[ $(id -u) -eq 0 || -n "${SYEDGE_ROOT:-}" ]] || die "请以 root 执行"
[[ -e "$M/lua" ]] || { echo "syedge-lua 未安装"; exit 0; }
for conf in "$ROOT/conf/current/syedge.conf" "$ROOT"/instances/*/conf/current/syedge.conf; do
  if grep -qs "modules/lua/" "$conf"; then
    die "当前配置正在使用 Lua 规则（${conf}）：先在控制台去掉 JWT 规则并发布到本节点，再卸载"
  fi
done
rm -f "$M/lua"
rm -rf "$M"/lua-* "$M"/.lua-*.tmp
echo "syedge-lua 已卸载"
