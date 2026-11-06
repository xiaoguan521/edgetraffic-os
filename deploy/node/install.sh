#!/usr/bin/env bash
# 边缘节点安装脚本（随节点发布包分发，规格书 5.2 / 9.4）。以 root 在解压目录中运行：
#
#   sudo ./install.sh                                   安装并以基线配置启动 syedge
#   sudo ./install.sh --server H:P --token T --ca-fingerprint F
#                                                       同时注册到控制台并启动 syedge-agent
#   sudo ./install.sh ... --replace-local-config         本机原为独立模式且有自定义配置时，备份后由控制台接管
#   sudo ./install.sh --firewall-ssh-from 10.0.0.0/8[,IP...]
#                                                       同时启用节点防火墙（可选，见 syedge-firewall）
#
# 可重复执行：已有的配置版本、注册身份都会保留，只更新程序文件。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SERVER=""; TOKEN=""; CAFP=""; FW_FROM=""; REPLACE_LOCAL=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server)         SERVER="$2"; shift 2 ;;
    --token)          TOKEN="$2";  shift 2 ;;
    --ca-fingerprint) CAFP="$2";   shift 2 ;;
    --firewall-ssh-from) FW_FROM="$2"; shift 2 ;;
    --replace-local-config) REPLACE_LOCAL=(-replace-local-config); shift ;;
    -h|--help)        sed -n '2,11p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done
die() { echo "install: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "请以 root 运行"
[[ -d "$HERE/rootfs/opt/syedge" ]] || die "请在解压后的发布包目录中运行"
command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || die "需要 systemd"
command -v sudo >/dev/null || die "缺少 sudo（apt install sudo / dnf install sudo）"

missing=$(ldd "$HERE/rootfs/opt/syedge/sbin/syedge" 2>&1 | awk '/not found/{print $1}' || true)
if [[ -n "$missing" ]]; then
  echo "缺少运行库：" $missing >&2
  if ldd "$HERE/rootfs/opt/syedge/sbin/syedge" 2>&1 | grep -q 'GLIBC_'; then
    echo "glibc 版本过低：需要 2.17 及以上" >&2
  fi
  exit 1
fi

echo "== 创建用户"
getent group syedge >/dev/null || groupadd -r syedge
id syedge >/dev/null 2>&1 || useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge
id syedge-agent >/dev/null 2>&1 || useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge-agent
# 可选节点防火墙的专用用户：开机加载与自动刷新规则时只带 CAP_NET_ADMIN（运行时不使用 root）
getent group syedge-firewall >/dev/null || groupadd -r syedge-firewall
id syedge-firewall >/dev/null 2>&1 || useradd -r -g syedge-firewall -d /nonexistent -s /usr/sbin/nologin syedge-firewall

echo "== 安装文件"
# 可执行文件先写临时文件再 rename：正在运行的程序不能原地覆盖（Text file busy），
# 运行中的 syedge 不受影响，新二进制在下次二进制热升级（USR2）时才生效。
put() { install -o root -g root -m 0755 -D "$1" "$2.new" && mv -f "$2.new" "$2"; }
mkdir -p /opt/syedge
# 默认文件与静态页；不覆盖 conf/ 下已有的配置版本
(cd "$HERE/rootfs/opt/syedge" && find . -path ./sbin -prune -o -type f -print) | while read -r f; do
  install -o root -g root -m 0644 -D "$HERE/rootfs/opt/syedge/$f" "/opt/syedge/$f"
done
mkdir -p /opt/syedge/conf/releases
put "$HERE/rootfs/opt/syedge/sbin/syedge" /opt/syedge/sbin/syedge
put "$HERE/rootfs/usr/bin/syedge-agent" /usr/bin/syedge-agent
put "$HERE/rootfs/usr/libexec/syedge/syedge-wrapper" /usr/libexec/syedge/syedge-wrapper
put "$HERE/rootfs/usr/sbin/syedge-firewall" /usr/sbin/syedge-firewall
put "$HERE/rootfs/usr/sbin/syedge-instance" /usr/sbin/syedge-instance
install -o root -g root -m 0644 "$HERE"/rootfs/etc/systemd/system/*.service "$HERE"/rootfs/etc/systemd/system/*.path /etc/systemd/system/
install -o root -g root -m 0644 "$HERE/rootfs/etc/logrotate.d/syedge" /etc/logrotate.d/syedge
install -o root -g root -m 0644 -D "$HERE/rootfs/etc/tmpfiles.d/syedge.conf" /etc/tmpfiles.d/syedge.conf
install -o root -g root -m 0440 "$HERE/rootfs/etc/sudoers.d/syedge-agent" /etc/sudoers.d/syedge-agent.new
visudo -cf /etc/sudoers.d/syedge-agent.new >/dev/null || { rm -f /etc/sudoers.d/syedge-agent.new; die "sudoers 校验失败"; }
mv /etc/sudoers.d/syedge-agent.new /etc/sudoers.d/syedge-agent
ln -sfn /opt/syedge/sbin/syedge /usr/sbin/syedge
chown -R root:root /opt/syedge/sbin /opt/syedge/share /opt/syedge/html

# syedge -t 会检查 pid 文件可写，/run/syedge 平时由 syedge.service 的 RuntimeDirectory 创建
mkdir -p /opt/syedge/conf/releases /var/log/syedge /var/cache/syedge /var/lib/syedge-agent /run/syedge-agent /run/syedge
chmod 0750 /var/lib/syedge-agent

echo "== 初始化配置"
if [[ ! -e /opt/syedge/conf/current ]]; then
  syedge-agent bootstrap
fi
chown -R syedge-agent:syedge /opt/syedge/conf /var/lib/syedge-agent /run/syedge-agent

echo "== 运行时不使用 root"
# 旧版本把防火墙规则放在 /etc/syedge：迁移到防火墙用户的目录
if [[ -f /etc/syedge/firewall.nft && ! -e /var/lib/syedge-firewall/firewall.nft ]]; then
  install -d -m 0700 -o syedge-firewall -g syedge-firewall /var/lib/syedge-firewall
  for f in firewall.nft firewall.nft.args; do
    [[ -f /etc/syedge/$f ]] && mv /etc/syedge/$f /var/lib/syedge-firewall/$f && chown syedge-firewall: /var/lib/syedge-firewall/$f
  done
fi
systemctl daemon-reload
# 交出运行时目录；升级前以 root 启动的 master 无中断切换为 syedge 运行（与二进制热升级同一机制，不重启）
/usr/sbin/syedge-instance nonroot
# 经 wrapper 以 syedge 校验（wrapper 带 CAP_NET_BIND_SERVICE，-t 需要绑定 80/443；不以 root 运行，也不会创建属于 root 的日志）
runuser -u syedge -- /usr/libexec/syedge/syedge-wrapper test "$(basename "$(readlink /opt/syedge/conf/current)")"

echo "== 启动 syedge"
systemctl daemon-reload
systemctl enable syedge.service >/dev/null 2>&1
if systemctl is-active -q syedge.service; then
  systemctl reload syedge.service   # 升级安装：只 reload，绝不 restart
else
  systemctl start syedge.service
fi
echo "$(/opt/syedge/sbin/syedge -v 2>&1)，systemd 单元 syedge.service 已启动"

if [[ -n "$SERVER$TOKEN$CAFP" ]]; then
  [[ -n "$SERVER" && -n "$TOKEN" && -n "$CAFP" ]] || die "--server、--token、--ca-fingerprint 需同时提供"
  echo "== 注册到控制台 $SERVER"
  syedge-agent enroll -server "$SERVER" -token "$TOKEN" -ca-fingerprint "$CAFP" ${REPLACE_LOCAL[@]+"${REPLACE_LOCAL[@]}"}
  chown -R syedge-agent:syedge /var/lib/syedge-agent
  systemctl enable --now syedge-agent.service
  echo "syedge-agent 已启动，几秒后节点会出现在控制台。"
elif [[ -f /var/lib/syedge-agent/node.json ]]; then
  systemctl enable syedge-agent.service >/dev/null 2>&1
  systemctl restart syedge-agent.service   # 只重启 agent 进程，不影响 syedge
  echo "已注册节点，syedge-agent 已更新并重启。"
else
  echo
  echo "下一步：在控制台“节点”页点“添加节点”，把生成的命令粘贴到本机执行。"
fi

if [[ -n "$FW_FROM" ]]; then
  echo "== 启用节点防火墙"
  /usr/sbin/syedge-firewall enable --ssh-from "$FW_FROM"
elif systemctl is-enabled -q syedge-firewall.service 2>/dev/null; then
  systemctl reload syedge-firewall.service   # 已启用：保持原规则
fi
