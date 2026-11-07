set -e
mkdir -p /var/lib/syedge-agent /run/syedge-agent /run/syedge /opt/syedge/conf/releases
chmod 0750 /var/lib/syedge-agent
if command -v visudo >/dev/null 2>&1; then
  if ! visudo -cf /etc/sudoers.d/syedge-agent >/dev/null 2>&1; then
    # CentOS 7 supports requiretty; newer sudo releases reject the obsolete
    # option, where the override is unnecessary.
    sed -i '/^Defaults:[^[:space:]]* !requiretty$/d' /etc/sudoers.d/syedge-agent
    visudo -cf /etc/sudoers.d/syedge-agent >/dev/null || { echo "syedge-agent: sudoers 校验失败" >&2; exit 1; }
  fi
fi
if [ ! -e /opt/syedge/conf/current ]; then
  /usr/bin/syedge-agent bootstrap
fi
chown -R syedge-agent:syedge /opt/syedge/conf /var/lib/syedge-agent /run/syedge-agent
# 旧版本把防火墙规则放在 /etc/syedge：迁移到防火墙用户的目录
if [ -f /etc/syedge/firewall.nft ] && [ ! -e /var/lib/syedge-firewall/firewall.nft ]; then
  install -d -m 0700 -o syedge-firewall -g syedge-firewall /var/lib/syedge-firewall
  for f in firewall.nft firewall.nft.args; do
    if [ -f /etc/syedge/$f ]; then mv /etc/syedge/$f /var/lib/syedge-firewall/$f && chown syedge-firewall: /var/lib/syedge-firewall/$f; fi
  done
fi
has_systemd && systemctl daemon-reload
# 运行时不使用 root：交出运行时目录；升级前以 root 启动的 master 无中断切换为 syedge 运行（不重启）
/usr/sbin/syedge-instance nonroot || echo "警告：syedge-instance nonroot 未完成（数据面照常服务），请检查后手工执行" >&2
# 经 wrapper 校验：默认以 syedge（wrapper 带 CAP_NET_BIND_SERVICE，-t 需要绑定 80/443）；
# 兼容模式（内核或 systemd 不支持 ambient 能力，如 CentOS 7）下 master 与 wrapper 以 root 运行
REV="$(basename "$(readlink /opt/syedge/conf/current)")"
if [ -f /etc/syedge/root-master ]; then
  /usr/libexec/syedge/syedge-wrapper test "$REV"
else
  runuser -u syedge -- /usr/libexec/syedge/syedge-wrapper test "$REV"
fi
if has_systemd; then
  systemctl daemon-reload
  systemctl enable syedge.service >/dev/null 2>&1 || true
  if systemctl is-active -q syedge.service; then
    systemctl reload syedge.service
  else
    systemctl start syedge.service
  fi
  if [ -f /var/lib/syedge-agent/node.json ]; then
    systemctl enable syedge-agent.service >/dev/null 2>&1 || true
    systemctl restart syedge-agent.service   # 只重启 agent，不影响 syedge
    echo "syedge-agent 已更新并重启。"
  else
    echo "syedge 已启动。下一步：在控制台“节点”页点“添加节点”，把生成的命令粘贴到本机执行。"
  fi
else
  echo "未检测到 systemd：请手工启动 /opt/syedge/sbin/syedge，注册后运行 syedge-agent run。"
fi
exit 0
