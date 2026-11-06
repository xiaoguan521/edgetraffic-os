mkdir -p /opt/syedge/conf/releases /var/log/syedge /var/cache/syedge /run/syedge /opt/syedge/versions /opt/syedge/modules
chown -R root:root /opt/syedge/sbin /opt/syedge/share /opt/syedge/html
# 运行时不使用 root（设计 0001 D5）：master 与 worker 以 syedge 运行，运行时目录、日志与缓存归 syedge；
# 二进制所在目录、版本库与模块目录由 wrapper（同样以 syedge 运行）在热升级时写入
chown -R syedge:syedge /var/log/syedge /var/cache/syedge /run/syedge /opt/syedge/versions
chown syedge:syedge /opt/syedge/sbin /opt/syedge/modules
if [ -x /usr/sbin/syedge-instance ]; then
  # 与 syedge-agent 同时升级：升级前以 root 启动的 master 无中断切换为 syedge 运行（旧版 agent 包没有该命令时忽略）
  /usr/sbin/syedge-instance nonroot >/dev/null 2>&1 || true
elif [ -f /run/syedge/syedge.pid ] && [ "$(awk '/^Uid:/{print $2}' /proc/"$(cat /run/syedge/syedge.pid)"/status 2>/dev/null)" = "0" ]; then
  echo "提示：运行中的 syedge master 仍以 root 运行（升级前启动）。安装 syedge-agent 后会自动无中断切换；" >&2
  echo "      独立使用时请在维护窗口执行 systemctl restart syedge，之后以 syedge 用户运行。" >&2
fi
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syedge.service; then
    # 升级：只 reload，绝不 restart。新二进制在下一次二进制热升级时生效，运行中的连接不受影响。
    systemctl reload syedge.service || true
  fi
fi
# 全新安装：写入初始版本 r000000（与 syedge-agent bootstrap 生成的完全相同），syedge 可独立使用。
# 已有 current（升级、或已由 agent 管理）时不改动任何配置。
if [ ! -e /opt/syedge/conf/current ] && [ -f /opt/syedge/share/defaults/baseline/syedge.conf ]; then
  if [ ! -e /opt/syedge/conf/releases/r000000 ]; then
    cp -a /opt/syedge/share/defaults/baseline /opt/syedge/conf/releases/r000000.new
    mv /opt/syedge/conf/releases/r000000.new /opt/syedge/conf/releases/r000000
  fi
  mkdir -p /opt/syedge/conf/releases/r000000/conf.d
  ln -sfn releases/r000000 /opt/syedge/conf/current
  echo "syedge 已安装，并写入了初始配置（未匹配域名的请求直接断开）。"
  echo "  独立使用：systemctl enable --now syedge，站点配置放在 /opt/syedge/conf/current/conf.d/，"
  echo "            示例 /opt/syedge/share/defaults/standalone-example.conf，说明 /opt/syedge/share/doc/STANDALONE.txt"
  echo "  接入控制台：安装 syedge-agent 后在控制台“节点”页添加节点。"
fi
exit 0
