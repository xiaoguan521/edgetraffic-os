refuse_remove_with_instances "$1"
if is_remove "$1"; then
  if has_systemd; then
    systemctl stop syedge.service >/dev/null 2>&1; systemctl disable syedge.service >/dev/null 2>&1 || true
    systemctl stop 'syedge@*.service' >/dev/null 2>&1 || true
  fi
  if [ -f /run/syedge/syedge.pid ] && [ -x /opt/syedge/sbin/syedge ]; then
    /opt/syedge/sbin/syedge -s quit >/dev/null 2>&1 || true
  fi
fi
exit 0
