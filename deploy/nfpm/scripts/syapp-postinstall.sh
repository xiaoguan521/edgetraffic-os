mkdir -p /opt/syapp/conf/releases /var/log/syapp /var/cache/syapp
chown -R root:root /opt/syapp/sbin /opt/syapp/share /opt/syapp/html
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syapp.service; then
    # 升级：只 reload，绝不 restart。新二进制在下一次二进制热升级时生效，运行中的连接不受影响。
    systemctl reload syapp.service || true
  fi
fi
if [ ! -e /opt/syapp/conf/current ]; then
  echo "syapp 已安装。安装 syapp-agent 后会写入初始配置并启动 syapp。"
fi
exit 0
