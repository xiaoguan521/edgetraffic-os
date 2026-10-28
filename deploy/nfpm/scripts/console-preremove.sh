if is_remove "$1" && has_systemd; then
  systemctl disable --now syapp-console.service >/dev/null 2>&1 || true
fi
exit 0
