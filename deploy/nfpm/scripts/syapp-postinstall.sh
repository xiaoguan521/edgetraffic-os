mkdir -p /opt/syapp/conf/releases /var/log/syapp /var/cache/syapp /run/syapp
chown -R root:root /opt/syapp/sbin /opt/syapp/share /opt/syapp/html
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syapp.service; then
    # 升级：只 reload，绝不 restart。新二进制在下一次二进制热升级时生效，运行中的连接不受影响。
    systemctl reload syapp.service || true
  fi
fi
# 全新安装：写入初始版本 r000000（与 syapp-agent bootstrap 生成的完全相同），syapp 可独立使用。
# 已有 current（升级、或已由 agent 管理）时不改动任何配置。
if [ ! -e /opt/syapp/conf/current ] && [ -f /opt/syapp/share/defaults/baseline/syapp.conf ]; then
  if [ ! -e /opt/syapp/conf/releases/r000000 ]; then
    cp -a /opt/syapp/share/defaults/baseline /opt/syapp/conf/releases/r000000.new
    mv /opt/syapp/conf/releases/r000000.new /opt/syapp/conf/releases/r000000
  fi
  mkdir -p /opt/syapp/conf/releases/r000000/conf.d
  ln -sfn releases/r000000 /opt/syapp/conf/current
  echo "syapp 已安装，并写入了初始配置（未匹配域名的请求直接断开）。"
  echo "  独立使用：systemctl enable --now syapp，站点配置放在 /opt/syapp/conf/current/conf.d/，"
  echo "            示例 /opt/syapp/share/defaults/standalone-example.conf，说明 /opt/syapp/share/doc/STANDALONE.txt"
  echo "  接入控制台：安装 syapp-agent 后在控制台“节点”页添加节点。"
fi
exit 0
