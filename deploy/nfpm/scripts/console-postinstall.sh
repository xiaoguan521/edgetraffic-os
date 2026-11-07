mkdir -p /var/lib/syedge-console
chown syedge-console:syedge-console /var/lib/syedge-console
chmod 0700 /var/lib/syedge-console
chgrp syedge-console /etc/syedge-console/console.env 2>/dev/null || true
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syedge-console.service; then
    systemctl restart syedge-console.service || true
    echo "syedge-console 已更新并重启。"
    exit 0
  fi
fi
echo "syedge-console 已安装。编辑 /etc/syedge-console/console.env（至少填写 ETOS_GRPC_HOSTS），然后执行："
echo "  systemctl enable syedge-console && systemctl start syedge-console"
exit 0
