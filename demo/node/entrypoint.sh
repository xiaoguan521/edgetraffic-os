#!/usr/bin/env bash
# 演示节点容器的入口：模拟一台装好 syedge 软件包的边缘服务器。
#  1. 首次启动写入初始版本 r000000 并启动 syedge（运行时不使用 root，设计 0001 D5：master 与 worker 以 syedge 运行，
#     只带 CAP_NET_BIND_SERVICE，监听 80/443）
#  2. 等待管理员在控制台“添加节点”后执行注册命令
#  3. 注册完成后以 syedge-agent 用户运行 agent（提权只经 sudo + syedge-wrapper）
set -euo pipefail
R=${SYEDGE_PREFIX:-/opt/syedge}
LOG_DIR="$R/logs"; CACHE_DIR="$R/cache"; STATE_DIR="$R/agent"; RUN_DIR="$R/run"
# 每个容器模拟一台独立的服务器：镜像中没有机器 ID，首次启动时生成一个（同机多实例检查以机器 ID 识别主机）
if [[ ! -s /etc/machine-id ]]; then
  tr -d '-' < /proc/sys/kernel/random/uuid > /etc/machine-id
fi
mkdir -p "$RUN_DIR" /run/syedge-agent "$LOG_DIR" "$CACHE_DIR" "$STATE_DIR" "$R/conf/releases"
chown -R syedge-agent:syedge "$R/conf" /run/syedge-agent "$STATE_DIR"
chmod 0750 "$STATE_DIR"

if [[ ! -e "$R/conf/current" ]]; then
  syedge-agent bootstrap -wrapper /usr/libexec/syedge/syedge-wrapper
  chown -R syedge-agent:syedge "$R/conf"
fi
mkdir -p "$R/versions" "$R/modules"
chown -R syedge:syedge "$RUN_DIR" "$LOG_DIR" "$CACHE_DIR" "$R/versions"
chown syedge:syedge "$R/sbin" "$R/modules"   # wrapper 热升级时在其中切换软链
chgrp -R syedge "$R/conf" && chmod -R g+rX "$R/conf"
asyedge() {
  setpriv --reuid=syedge --regid=syedge --init-groups \
    --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- "$R/sbin/syedge" -p "$R" -e "$LOG_DIR/error.log" -c "$R/conf/current/syedge.conf" "$@"
}
asyedge -t -q
asyedge
echo "syedge started: $("$R/sbin/syedge" -v 2>&1)"

trap 'asyedge -s quit; exit 0' TERM INT

until [[ -f "$STATE_DIR/node.json" ]]; do
  sleep 2 &
  wait $!
done
chown -R syedge-agent:syedge "$STATE_DIR"
echo "node enrolled, starting syedge-agent"
setpriv --reuid=syedge-agent --regid=syedge --init-groups -- syedge-agent run &
wait $!
