#!/usr/bin/env bash
# 演示节点容器的入口：模拟一台装好 syapp 软件包的边缘服务器。
#  1. 首次启动写入初始版本 r000000 并启动 syapp（运行时不使用 root，设计 0001 D5：master 与 worker 以 syapp 运行，
#     只带 CAP_NET_BIND_SERVICE，监听 80/443）
#  2. 等待管理员在控制台“添加节点”后执行注册命令
#  3. 注册完成后以 syapp-agent 用户运行 agent（提权只经 sudo + syapp-wrapper）
set -euo pipefail
mkdir -p /run/syapp /run/syapp-agent /var/log/syapp /var/cache/syapp /var/lib/syapp-agent /opt/syapp/conf/releases
chown -R syapp-agent:syapp /opt/syapp/conf /run/syapp-agent /var/lib/syapp-agent
chmod 0750 /var/lib/syapp-agent

if [[ ! -e /opt/syapp/conf/current ]]; then
  syapp-agent bootstrap -wrapper /usr/libexec/syapp/syapp-wrapper
  chown -R syapp-agent:syapp /opt/syapp/conf
fi
mkdir -p /opt/syapp/versions /opt/syapp/modules
chown -R syapp:syapp /run/syapp /var/log/syapp /var/cache/syapp /opt/syapp/versions
chown syapp:syapp /opt/syapp/sbin /opt/syapp/modules   # wrapper 热升级时在其中切换软链
chgrp -R syapp /opt/syapp/conf && chmod -R g+rX /opt/syapp/conf
asyapp() {
  setpriv --reuid=syapp --regid=syapp --init-groups \
    --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- /opt/syapp/sbin/syapp "$@"
}
asyapp -t -q
asyapp
echo "syapp started: $(/opt/syapp/sbin/syapp -v 2>&1)"

trap 'asyapp -s quit; exit 0' TERM INT

until [[ -f /var/lib/syapp-agent/node.json ]]; do
  sleep 2 &
  wait $!
done
chown -R syapp-agent:syapp /var/lib/syapp-agent
echo "node enrolled, starting syapp-agent"
setpriv --reuid=syapp-agent --regid=syapp --init-groups -- syapp-agent run &
wait $!
