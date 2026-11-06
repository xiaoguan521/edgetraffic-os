mkdir -p /opt/syapp/conf/releases /var/log/syapp /var/cache/syapp /run/syapp /opt/syapp/versions /opt/syapp/modules
chown -R root:root /opt/syapp/sbin /opt/syapp/share /opt/syapp/html
# 运行时不使用 root（设计 0001 D5）：master 与 worker 以 syapp 运行，运行时目录、日志与缓存归 syapp；
# 二进制所在目录、版本库与模块目录由 wrapper（同样以 syapp 运行）在热升级时写入
chown -R syapp:syapp /var/log/syapp /var/cache/syapp /run/syapp /opt/syapp/versions
chown syapp:syapp /opt/syapp/sbin /opt/syapp/modules
if [ -x /usr/sbin/syapp-instance ]; then
  # 与 syapp-agent 同时升级：升级前以 root 启动的 master 无中断切换为 syapp 运行（旧版 agent 包没有该命令时忽略）
  /usr/sbin/syapp-instance nonroot >/dev/null 2>&1 || true
elif [ -f /run/syapp/syapp.pid ] && [ "$(awk '/^Uid:/{print $2}' /proc/"$(cat /run/syapp/syapp.pid)"/status 2>/dev/null)" = "0" ]; then
  echo "提示：运行中的 syapp master 仍以 root 运行（升级前启动）。安装 syapp-agent 后会自动无中断切换；" >&2
  echo "      独立使用时请在维护窗口执行 systemctl restart syapp，之后以 syapp 用户运行。" >&2
fi
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
