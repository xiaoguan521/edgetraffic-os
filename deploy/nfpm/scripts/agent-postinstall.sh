set -e
mkdir -p /var/lib/syapp-agent /run/syapp-agent /run/syapp /opt/syapp/conf/releases
chmod 0750 /var/lib/syapp-agent
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/syapp-agent >/dev/null || { echo "syapp-agent: sudoers 校验失败" >&2; exit 1; }
fi
if [ ! -e /opt/syapp/conf/current ]; then
  /usr/bin/syapp-agent bootstrap
fi
chown -R syapp-agent:syapp /opt/syapp/conf /var/lib/syapp-agent /run/syapp-agent
# 旧版本把防火墙规则放在 /etc/syapp：迁移到防火墙用户的目录
if [ -f /etc/syapp/firewall.nft ] && [ ! -e /var/lib/syapp-firewall/firewall.nft ]; then
  install -d -m 0700 -o syapp-firewall -g syapp-firewall /var/lib/syapp-firewall
  for f in firewall.nft firewall.nft.args; do
    if [ -f /etc/syapp/$f ]; then mv /etc/syapp/$f /var/lib/syapp-firewall/$f && chown syapp-firewall: /var/lib/syapp-firewall/$f; fi
  done
fi
has_systemd && systemctl daemon-reload
# 运行时不使用 root：交出运行时目录；升级前以 root 启动的 master 无中断切换为 syapp 运行（不重启）
/usr/sbin/syapp-instance nonroot || echo "警告：syapp-instance nonroot 未完成（数据面照常服务），请检查后手工执行" >&2
runuser -u syapp -- /opt/syapp/sbin/syapp -t -q
if has_systemd; then
  systemctl daemon-reload
  systemctl enable syapp.service >/dev/null 2>&1 || true
  if systemctl is-active -q syapp.service; then
    systemctl reload syapp.service
  else
    systemctl start syapp.service
  fi
  if [ -f /var/lib/syapp-agent/node.json ]; then
    systemctl enable syapp-agent.service >/dev/null 2>&1 || true
    systemctl restart syapp-agent.service   # 只重启 agent，不影响 syapp
    echo "syapp-agent 已更新并重启。"
  else
    echo "syapp 已启动。下一步：在控制台“节点”页点“添加节点”，把生成的命令粘贴到本机执行。"
  fi
else
  echo "未检测到 systemd：请手工启动 /opt/syapp/sbin/syapp，注册后运行 syapp-agent run。"
fi
exit 0
