if is_remove "$1" && has_systemd; then
  systemctl stop syedge-console.service >/dev/null 2>&1; systemctl disable syedge-console.service >/dev/null 2>&1 || true
fi
exit 0
