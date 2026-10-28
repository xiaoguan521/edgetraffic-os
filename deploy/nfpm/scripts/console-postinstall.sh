mkdir -p /var/lib/syapp-console
chown syapp-console:syapp-console /var/lib/syapp-console
chmod 0700 /var/lib/syapp-console
chgrp syapp-console /etc/syapp-console/console.env 2>/dev/null || true
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syapp-console.service; then
    systemctl restart syapp-console.service || true
    echo "syapp-console 已更新并重启。"
    exit 0
  fi
fi
echo "syapp-console 已安装。编辑 /etc/syapp-console/console.env（至少填写 ETOS_GRPC_HOSTS），然后执行："
echo "  systemctl enable --now syapp-console"
exit 0
