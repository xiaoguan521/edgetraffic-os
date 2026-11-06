#!/usr/bin/env bash
# 边缘节点卸载脚本（随节点发布包分发）。以 root 运行：
#
#   sudo ./uninstall.sh           停止服务并移除程序与 systemd 单元（保留配置版本与日志）
#   sudo ./uninstall.sh --purge   同时清除 /opt/syedge、/var/lib/syedge-agent、/var/log/syedge、/var/cache/syedge
#   sudo ./uninstall.sh --instance <名称> [--purge]   只删除一个同机实例（等同 syedge-instance remove <名称> --yes）
#   sudo ./uninstall.sh --all [--purge]   先删除全部同机实例，再卸载节点
set -euo pipefail

PURGE=0; INSTANCE=""; ALL=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    --instance) INSTANCE="${2:-}"; shift 2 ;;
    --all) ALL=1; shift ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "uninstall: 请以 root 运行" >&2; exit 1; }

purge_arg=(); [[ $PURGE -eq 1 ]] && purge_arg=(--purge)
if [[ -n "$INSTANCE" ]]; then
  exec /usr/sbin/syedge-instance remove "$INSTANCE" --yes "${purge_arg[@]+"${purge_arg[@]}"}"
fi
insts=()
for f in /opt/syedge/instances/*/instance.json; do [[ -f "$f" ]] && insts+=("$(basename "$(dirname "$f")")"); done
if [[ ${#insts[@]} -gt 0 ]]; then
  if [[ $ALL -ne 1 ]]; then
    echo "本机还有同机多实例：${insts[*]}" >&2
    echo "请先用 --instance <名称> 逐个删除，或加 --all 一并删除" >&2
    exit 1
  fi
  for i in "${insts[@]}"; do /usr/sbin/syedge-instance remove "$i" --yes "${purge_arg[@]+"${purge_arg[@]}"}"; done
fi

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl disable --now syedge-agent.service >/dev/null 2>&1 || true
  systemctl stop 'syedge-agent@*.service' >/dev/null 2>&1 || true
  systemctl disable --now syedge.service >/dev/null 2>&1 || true
  systemctl stop 'syedge@*.service' >/dev/null 2>&1 || true
  if systemctl is-enabled -q syedge-firewall.service 2>/dev/null || systemctl is-active -q syedge-firewall.service 2>/dev/null; then
    /usr/sbin/syedge-firewall disable >/dev/null 2>&1 || true
  fi
fi

if [[ -f /run/syedge/syedge.pid && -x /opt/syedge/sbin/syedge ]]; then
  /opt/syedge/sbin/syedge -s quit >/dev/null 2>&1 || true
fi

rm -f /usr/sbin/syedge /usr/bin/syedge-agent /usr/libexec/syedge/syedge-wrapper /usr/sbin/syedge-firewall
rm -f /etc/sudoers.d/syedge-agent /etc/logrotate.d/syedge /etc/tmpfiles.d/syedge.conf
rm -f /etc/systemd/system/syedge.service /etc/systemd/system/syedge@.service \
      /etc/systemd/system/syedge-agent.service /etc/systemd/system/syedge-agent@.service \
      /etc/systemd/system/syedge-firewall.service /etc/systemd/system/syedge-firewall-refresh.service \
      /etc/systemd/system/syedge-firewall-refresh.path
rm -rf /etc/systemd/system/syedge-firewall-refresh.path.d
rm -rf /opt/syedge/sbin /opt/syedge/versions /opt/syedge/modules

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [[ $PURGE -eq 1 ]]; then
  rm -rf /opt/syedge /var/lib/syedge-agent /var/lib/syedge-agent-* /var/lib/syedge-firewall /var/log/syedge /var/cache/syedge /etc/syedge-agent /run/syedge /run/syedge-* /run/syedge-agent /run/syedge-agent-*
  echo "已卸载 SyEdge 节点并清理全部配置与数据（--purge）。"
else
  echo "已卸载 SyEdge 节点程序（保留 /opt/syedge/conf 与 /var/log/syedge；如需彻底清除请加 --purge）。"
fi

