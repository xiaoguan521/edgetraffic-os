#!/usr/bin/env bash
# 演示节点容器的入口：模拟一台装好 syedge 软件包的边缘服务器。
#  1. 首次启动写入初始版本 r000000 并启动 syedge（运行时不使用 root，设计 0001 D5：master 与 worker 以 syedge 运行，
#     只带 CAP_NET_BIND_SERVICE，监听 80/443）
#  2. 等待管理员在控制台“添加节点”后执行注册命令
#  3. 注册完成后以 syedge-agent 用户运行 agent（提权只经 sudo + syedge-wrapper）
set -euo pipefail
# 每个容器模拟一台独立的服务器：镜像中没有机器 ID，首次启动时生成一个（同机多实例检查以机器 ID 识别主机）
if [[ ! -s /etc/machine-id ]]; then
  tr -d '-' < /proc/sys/kernel/random/uuid > /etc/machine-id
fi
mkdir -p /run/syedge /run/syedge-agent /var/log/syedge /var/cache/syedge /var/lib/syedge-agent /opt/syedge/conf/releases
chown -R syedge-agent:syedge /opt/syedge/conf /run/syedge-agent /var/lib/syedge-agent
chmod 0750 /var/lib/syedge-agent

if [[ ! -e /opt/syedge/conf/current ]]; then
  syedge-agent bootstrap -wrapper /usr/libexec/syedge/syedge-wrapper
  chown -R syedge-agent:syedge /opt/syedge/conf
fi
mkdir -p /opt/syedge/versions /opt/syedge/modules
chown -R syedge:syedge /run/syedge /var/log/syedge /var/cache/syedge /opt/syedge/versions
chown syedge:syedge /opt/syedge/sbin /opt/syedge/modules   # wrapper 热升级时在其中切换软链
chgrp -R syedge /opt/syedge/conf && chmod -R g+rX /opt/syedge/conf
asyedge() {
  setpriv --reuid=syedge --regid=syedge --init-groups \
    --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- /opt/syedge/sbin/syedge "$@"
}
asyedge -t -q
asyedge
echo "syedge started: $(/opt/syedge/sbin/syedge -v 2>&1)"

trap 'asyedge -s quit; exit 0' TERM INT

until [[ -f /var/lib/syedge-agent/node.json ]]; do
  sleep 2 &
  wait $!
done
chown -R syedge-agent:syedge /var/lib/syedge-agent
echo "node enrolled, starting syedge-agent"
setpriv --reuid=syedge-agent --regid=syedge --init-groups -- syedge-agent run &
wait $!
