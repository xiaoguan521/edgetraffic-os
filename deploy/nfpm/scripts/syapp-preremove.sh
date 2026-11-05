if is_remove "$1"; then
  if has_systemd; then
    systemctl disable --now syapp.service >/dev/null 2>&1 || true
    systemctl stop 'syapp@*.service' >/dev/null 2>&1 || true
  fi
  if [ -f /run/syapp/syapp.pid ] && [ -x /opt/syapp/sbin/syapp ]; then
    /opt/syapp/sbin/syapp -s quit >/dev/null 2>&1 || true
  fi
fi
exit 0
