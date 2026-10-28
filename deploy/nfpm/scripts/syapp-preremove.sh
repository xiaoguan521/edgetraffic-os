if is_remove "$1"; then
  if has_systemd; then systemctl disable syapp.service >/dev/null 2>&1 || true; fi
  # 卸载软件包不停止正在运行的 syapp，避免业务中断；需要时由管理员手工停止。
  echo "syapp 软件包将被卸载；正在运行的 syapp 不会被停止。如需停止：systemctl stop syapp"
fi
exit 0
