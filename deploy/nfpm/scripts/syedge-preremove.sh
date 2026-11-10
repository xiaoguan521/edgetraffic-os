refuse_remove_with_instances "$1" || exit 1
if is_remove "$1"; then
  r=$(syedge_root) || exit 1
  if has_systemd; then
    systemctl stop syedge.service >/dev/null 2>&1; systemctl disable syedge.service >/dev/null 2>&1 || true
    systemctl stop 'syedge@*.service' >/dev/null 2>&1 || true
  fi
  if [ -f /run/syedge/syedge.pid ] && [ -x "$r/sbin/syedge" ]; then
    log="$r/logs/error.log"
    [ "$r" != "$LEGACY_ROOT" ] || log=/var/log/syedge/error.log
    "$r/sbin/syedge" -p "$r" -e "$log" -c "$r/conf/current/syedge.conf" -s quit >/dev/null 2>&1 || true
  fi
fi
exit 0
