refuse_remove_with_instances "$1" || exit 1
if is_remove "$1" && has_systemd; then
  systemctl stop syedge-agent.service >/dev/null 2>&1; systemctl disable syedge-agent.service >/dev/null 2>&1 || true
fi
exit 0
