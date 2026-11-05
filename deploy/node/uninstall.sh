#!/usr/bin/env bash
# 边缘节点卸载脚本（随节点发布包分发）。以 root 运行：
#
#   sudo ./uninstall.sh           停止服务并移除程序与 systemd 单元（保留配置版本与日志）
#   sudo ./uninstall.sh --purge   同时清除 /opt/syapp、/var/lib/syapp-agent、/var/log/syapp、/var/cache/syapp
set -euo pipefail

PURGE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,5p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "uninstall: 请以 root 运行" >&2; exit 1; }

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl disable --now syapp-agent.service >/dev/null 2>&1 || true
  systemctl stop 'syapp-agent@*.service' >/dev/null 2>&1 || true
  systemctl disable --now syapp.service >/dev/null 2>&1 || true
  systemctl stop 'syapp@*.service' >/dev/null 2>&1 || true
  if systemctl is-enabled -q syapp-firewall.service 2>/dev/null || systemctl is-active -q syapp-firewall.service 2>/dev/null; then
    /usr/sbin/syapp-firewall disable >/dev/null 2>&1 || true
  fi
fi

if [[ -f /run/syapp/syapp.pid && -x /opt/syapp/sbin/syapp ]]; then
  /opt/syapp/sbin/syapp -s quit >/dev/null 2>&1 || true
fi

rm -f /usr/sbin/syapp /usr/bin/syapp-agent /usr/libexec/syapp/syapp-wrapper /usr/sbin/syapp-firewall
rm -f /etc/sudoers.d/syapp-agent /etc/logrotate.d/syapp /etc/tmpfiles.d/syapp.conf
rm -f /etc/systemd/system/syapp.service /etc/systemd/system/syapp@.service \
      /etc/systemd/system/syapp-agent.service /etc/systemd/system/syapp-agent@.service \
      /etc/systemd/system/syapp-firewall.service
rm -rf /opt/syapp/sbin /opt/syapp/versions /opt/syapp/modules

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [[ $PURGE -eq 1 ]]; then
  rm -rf /opt/syapp /var/lib/syapp-agent /var/lib/syapp-agent-* /var/log/syapp /var/cache/syapp /etc/syapp-agent /run/syapp /run/syapp-* /run/syapp-agent /run/syapp-agent-*
  echo "已卸载 EdgeTraffic OS 节点并清理全部配置与数据（--purge）。"
else
  echo "已卸载 EdgeTraffic OS 节点程序（保留 /opt/syapp/conf 与 /var/log/syapp；如需彻底清除请加 --purge）。"
fi

