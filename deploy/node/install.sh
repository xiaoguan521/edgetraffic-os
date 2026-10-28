#!/usr/bin/env bash
# 边缘节点安装脚本（随节点发布包分发，规格书 5.2 / 9.4）。以 root 在解压目录中运行：
#
#   sudo ./install.sh                                   安装并以基线配置启动 syapp
#   sudo ./install.sh --server H:P --token T --ca-fingerprint F
#                                                       同时注册到控制台并启动 syapp-agent
#
# 可重复执行：已有的配置版本、注册身份都会保留，只更新程序文件。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SERVER=""; TOKEN=""; CAFP=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server)         SERVER="$2"; shift 2 ;;
    --token)          TOKEN="$2";  shift 2 ;;
    --ca-fingerprint) CAFP="$2";   shift 2 ;;
    -h|--help)        sed -n '2,8p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done
die() { echo "install: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "请以 root 运行"
[[ -d "$HERE/rootfs/opt/syapp" ]] || die "请在解压后的发布包目录中运行"
command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || die "需要 systemd"
command -v sudo >/dev/null || die "缺少 sudo（apt install sudo / dnf install sudo）"

missing=$(ldd "$HERE/rootfs/opt/syapp/sbin/syapp" 2>&1 | awk '/not found/{print $1}' || true)
if [[ -n "$missing" ]]; then
  echo "缺少运行库：" $missing >&2
  echo "Ubuntu / Debian：apt install libpcre2-8-0 libssl3 zlib1g" >&2
  if ldd "$HERE/rootfs/opt/syapp/sbin/syapp" 2>&1 | grep -q 'GLIBC_'; then
    echo "glibc 版本过低：需要 2.35 及以上" >&2
  fi
  exit 1
fi

echo "== 创建用户"
getent group syapp >/dev/null || groupadd -r syapp
id syapp >/dev/null 2>&1 || useradd -r -g syapp -d /nonexistent -s /usr/sbin/nologin syapp
id syapp-agent >/dev/null 2>&1 || useradd -r -g syapp -d /nonexistent -s /usr/sbin/nologin syapp-agent

echo "== 安装文件"
# 程序与默认文件；不覆盖 conf/ 下已有的配置版本
cp -a "$HERE/rootfs/opt/syapp/." /opt/syapp/ 2>/dev/null || { mkdir -p /opt/syapp && cp -a "$HERE/rootfs/opt/syapp/." /opt/syapp/; }
install -o root -g root -m 0755 "$HERE/rootfs/usr/bin/syapp-agent" /usr/bin/syapp-agent
install -o root -g root -m 0755 -D "$HERE/rootfs/usr/libexec/syapp/syapp-wrapper" /usr/libexec/syapp/syapp-wrapper
install -o root -g root -m 0644 "$HERE"/rootfs/etc/systemd/system/*.service /etc/systemd/system/
install -o root -g root -m 0644 "$HERE/rootfs/etc/logrotate.d/syapp" /etc/logrotate.d/syapp
install -o root -g root -m 0440 "$HERE/rootfs/etc/sudoers.d/syapp-agent" /etc/sudoers.d/syapp-agent.new
visudo -cf /etc/sudoers.d/syapp-agent.new >/dev/null || { rm -f /etc/sudoers.d/syapp-agent.new; die "sudoers 校验失败"; }
mv /etc/sudoers.d/syapp-agent.new /etc/sudoers.d/syapp-agent
ln -sfn /opt/syapp/sbin/syapp /usr/sbin/syapp
chown -R root:root /opt/syapp/sbin /opt/syapp/share /opt/syapp/html

# syapp -t 会检查 pid 文件可写，/run/syapp 平时由 syapp.service 的 RuntimeDirectory 创建
mkdir -p /opt/syapp/conf/releases /var/log/syapp /var/cache/syapp /var/lib/syapp-agent /run/syapp-agent /run/syapp
chmod 0750 /var/lib/syapp-agent

echo "== 初始化配置"
if [[ ! -e /opt/syapp/conf/current ]]; then
  syapp-agent bootstrap
fi
chown -R syapp-agent:syapp /opt/syapp/conf /var/lib/syapp-agent /run/syapp-agent
/opt/syapp/sbin/syapp -t -q

echo "== 启动 syapp"
systemctl daemon-reload
systemctl enable syapp.service >/dev/null 2>&1
if systemctl is-active -q syapp.service; then
  systemctl reload syapp.service   # 升级安装：只 reload，绝不 restart
else
  systemctl start syapp.service
fi
echo "$(/opt/syapp/sbin/syapp -v 2>&1)，systemd 单元 syapp.service 已启动"

if [[ -n "$SERVER$TOKEN$CAFP" ]]; then
  [[ -n "$SERVER" && -n "$TOKEN" && -n "$CAFP" ]] || die "--server、--token、--ca-fingerprint 需同时提供"
  echo "== 注册到控制台 $SERVER"
  syapp-agent enroll -server "$SERVER" -token "$TOKEN" -ca-fingerprint "$CAFP"
  chown -R syapp-agent:syapp /var/lib/syapp-agent
  systemctl enable --now syapp-agent.service
  echo "syapp-agent 已启动，几秒后节点会出现在控制台。"
elif [[ -f /var/lib/syapp-agent/node.json ]]; then
  systemctl enable syapp-agent.service >/dev/null 2>&1
  systemctl restart syapp-agent.service   # 只重启 agent 进程，不影响 syapp
  echo "已注册节点，syapp-agent 已更新并重启。"
else
  echo
  echo "下一步：在控制台“节点”页点“添加节点”，把生成的命令粘贴到本机执行。"
fi
