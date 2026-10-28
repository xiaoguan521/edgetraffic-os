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
/opt/syapp/sbin/syapp -t -q
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
