#!/usr/bin/env bash
# 边缘节点安装脚本（随节点发布包分发，规格书 5.2 / 9.4）。以 root 在解压目录中运行：
#
#   sudo ./install.sh                                   安装并以基线配置启动 syedge
#   sudo ./install.sh --server H:P --token T --ca-fingerprint F
#                                                       同时注册到控制台并启动 syedge-agent
#   sudo ./install.sh ... --replace-local-config         本机原为独立模式且有自定义配置时，备份后由控制台接管
#   sudo ./install.sh --prefix /data/syedge                    指定 system 安装根目录（只允许首次安装）
#   sudo ./install.sh --firewall-ssh-from 10.0.0.0/8[,IP...]
#                                                       同时启用节点防火墙（可选，见 syedge-firewall）
#   sudo ./install.sh --root-master                     兼容模式：与传统 Web 服务器相同由 root 运行 master（worker 仍以 syedge 运行）。
#                                                       内核或 systemd 不支持 ambient 能力（如 CentOS 7）时自动使用
#
# 可重复执行：已有的配置版本、注册身份都会保留，只更新程序文件。
set -euo pipefail
# 配置需要供 syedge 组读取；密钥等敏感文件仍按各自显式权限创建。
umask 022
HERE="$(cd "$(dirname "$0")" && pwd)"
LEGACY_ROOT=/opt/"syedge" # 不随编译前缀模板替换；旧版本使用 /var 下的运行目录
SERVER=""; TOKEN=""; CAFP=""; FW_FROM=""; PREFIX_ARG=""; REPLACE_LOCAL=(); ROOT_MASTER=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server)         SERVER="$2"; shift 2 ;;
    --token)          TOKEN="$2";  shift 2 ;;
    --ca-fingerprint) CAFP="$2";   shift 2 ;;
    --prefix)         [[ $# -ge 2 ]] || { echo "--prefix 需要一个安装根目录" >&2; exit 2; }; PREFIX_ARG="$2"; shift 2 ;;
    --firewall-ssh-from) FW_FROM="$2"; shift 2 ;;
    --replace-local-config) REPLACE_LOCAL=(-replace-local-config); shift ;;
    --root-master)    ROOT_MASTER=(--root-master); shift ;;
    -h|--help)        sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done
die() { echo "install: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "请以 root 运行"
[[ -d "$HERE/rootfs/opt/syedge" ]] || die "请在解压后的发布包目录中运行"
command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || die "需要 systemd"
command -v sudo >/dev/null || die "缺少 sudo（apt install sudo / dnf install sudo）"
command -v runuser >/dev/null || die "缺少 runuser（apt install util-linux / dnf install util-linux）"

missing=$(ldd "$HERE/rootfs/opt/syedge/sbin/syedge" 2>&1 | awk '/not found/{print $1}' || true)
if [[ -n "$missing" ]]; then
  echo "缺少运行库：" $missing >&2
  if ldd "$HERE/rootfs/opt/syedge/sbin/syedge" 2>&1 | grep -q 'GLIBC_'; then
    echo "glibc 版本过低：需要 2.17 及以上" >&2
  fi
  exit 1
fi

# 在覆盖任何已安装程序前校验根目录及重复安装参数。
if [[ -n "$PREFIX_ARG" ]]; then
  "$HERE/rootfs/usr/sbin/syedge-instance" prefix "$PREFIX_ARG" >/dev/null
fi
R="$("$HERE/rootfs/usr/sbin/syedge-instance" prefix)"
[[ -n "$R" && "$R" != "/" ]] || die "安装根目录无效"
for d in "$R" "$R/sbin" "$R/share" "$R/html" "$R/modules" "$R/versions"; do
  [[ ! -L "$d" ]] || die "安装目录不能是符号链接：$d"
done

echo "== 创建用户"
getent group syedge >/dev/null || groupadd -r syedge
id syedge >/dev/null 2>&1 || useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge
id syedge-agent >/dev/null 2>&1 || useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge-agent
# 可选节点防火墙的专用用户：开机加载与自动刷新规则时只带 CAP_NET_ADMIN（运行时不使用 root）
getent group syedge-firewall >/dev/null || groupadd -r syedge-firewall
id syedge-firewall >/dev/null 2>&1 || useradd -r -g syedge-firewall -d /nonexistent -s /usr/sbin/nologin syedge-firewall

if [[ ! -d "$R" ]]; then
  install -d -o root -g root -m 0755 "$R"
fi
for runtime_user in syedge syedge-agent; do
  runuser -u "$runtime_user" -- test -x "$R" || die "$runtime_user 无法访问安装根目录 $R，请检查父目录权限"
  if [[ -e /etc/syedge/prefix ]]; then
    runuser -u "$runtime_user" -- test -r /etc/syedge/prefix || die "$runtime_user 无法读取 /etc/syedge/prefix，请检查父目录权限"
  fi
done

echo "== 安装文件"
# 可执行文件先写临时文件再 rename：正在运行的程序不能原地覆盖（Text file busy），
# 运行中的 syedge 不受影响，新二进制在下次二进制热升级（USR2）时才生效。
put() { install -o root -g root -m 0755 -D "$1" "$2.new" && mv -f "$2.new" "$2"; }
put "$HERE/rootfs/usr/bin/syedge-agent" /usr/bin/syedge-agent
put "$HERE/rootfs/usr/libexec/syedge/syedge-wrapper" /usr/libexec/syedge/syedge-wrapper
put "$HERE/rootfs/usr/sbin/syedge-firewall" /usr/sbin/syedge-firewall
put "$HERE/rootfs/usr/sbin/syedge-instance" /usr/sbin/syedge-instance

if [[ "$R" == "$LEGACY_ROOT" ]]; then
  LOG_DIR=/var/log/syedge
  CACHE_DIR=/var/cache/syedge
  STATE_DIR=/var/lib/syedge-agent
else
  LOG_DIR="$R/logs"
  CACHE_DIR="$R/cache"
  STATE_DIR="$R/agent"
fi

# 默认文件与静态页；不覆盖 conf/ 下已有的配置版本
(cd "$HERE/rootfs/opt/syedge" && find . -path ./sbin -prune -o -type f -print) | while read -r f; do
  if [[ "$f" == ./html/* && ( -e "$R/$f" || -L "$R/$f" ) ]]; then
    continue # 独立站点静态内容由管理员维护，重复安装不能覆盖
  fi
  install -o root -g root -m 0644 -D "$HERE/rootfs/opt/syedge/$f" "$R/$f"
done
if [[ "$R" != "/opt/syedge" && -f "$R/share/defaults/standalone-example.conf" ]]; then
  sed -i "s|/opt/syedge|$R|g; s|root   html;|root   $R/html;|g" "$R/share/defaults/standalone-example.conf"
fi
mkdir -p "$R/conf/releases"
# sbin 目录运行时由 syedge 拥有；在 root 所有的安装根目录中创建临时文件，
# 避免可预测临时文件被替换，同时以 rename 更新正在运行的二进制。
mkdir -p "$R/sbin"
dp_tmp=$(mktemp "$R/.syedge-bin.XXXXXX")
if ! install -o root -g root -m 0755 "$HERE/rootfs/opt/syedge/sbin/syedge" "$dp_tmp" || ! mv -f "$dp_tmp" "$R/sbin/syedge"; then
  rm -f "$dp_tmp"
  die "安装数据面二进制失败"
fi
install -o root -g root -m 0644 "$HERE"/rootfs/etc/systemd/system/*.service "$HERE"/rootfs/etc/systemd/system/*.path /etc/systemd/system/
if [[ ! -e /etc/logrotate.d/syedge ]]; then
  install -o root -g root -m 0644 "$HERE/rootfs/etc/logrotate.d/syedge" /etc/logrotate.d/syedge
fi
install -o root -g root -m 0644 -D "$HERE/rootfs/etc/tmpfiles.d/syedge.conf" /etc/tmpfiles.d/syedge.conf
sudoers_new=/etc/sudoers.d/syedge-agent.new
install -o root -g root -m 0440 "$HERE/rootfs/etc/sudoers.d/syedge-agent" "$sudoers_new"
if ! visudo -cf "$sudoers_new" >/dev/null 2>&1; then
  # CentOS 7 supports requiretty; newer sudo releases reject the obsolete
  # option, where the override is unnecessary.
  sed -i '/^Defaults:[^[:space:]]* !requiretty$/d' "$sudoers_new"
  visudo -cf "$sudoers_new" >/dev/null || { rm -f "$sudoers_new"; die "sudoers 校验失败"; }
fi
mv /etc/sudoers.d/syedge-agent.new /etc/sudoers.d/syedge-agent
ln -sfn "$R/sbin/syedge" /usr/sbin/syedge
chown -R root:root "$R/sbin" "$R/share"
chown root:root "$R/html"

# syedge -t 会检查 pid 文件可写，/run/syedge 平时由 syedge.service 的 RuntimeDirectory 创建
mkdir -p "$R/conf/releases" "$LOG_DIR" "$CACHE_DIR" "$STATE_DIR" /run/syedge-agent /run/syedge
chmod 0750 "$STATE_DIR"

echo "== 初始化配置"
if [[ ! -e "$R/conf/current" ]]; then
  syedge-agent bootstrap
fi
chown -R syedge-agent:syedge "$R/conf" "$STATE_DIR" /run/syedge-agent

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
/usr/sbin/syedge-instance nonroot ${ROOT_MASTER[@]+"${ROOT_MASTER[@]}"}
# 经 wrapper 校验：默认以 syedge（wrapper 带 CAP_NET_BIND_SERVICE，-t 需要绑定 80/443；不会创建属于 root 的日志）；
# 兼容模式下 master 以 root 运行，wrapper 也以 root 运行
REV="$(basename "$(readlink "$R/conf/current")")"
if [[ -f /etc/syedge/root-master ]]; then
  /usr/libexec/syedge/syedge-wrapper test "$REV"
else
  runuser -u syedge -- /usr/libexec/syedge/syedge-wrapper test "$REV"
fi

echo "== 启动 syedge"
systemctl daemon-reload
systemctl enable syedge.service >/dev/null 2>&1
if systemctl is-active -q syedge.service; then
  systemctl reload syedge.service   # 升级安装：只 reload，绝不 restart
else
  systemctl start syedge.service
fi
echo "$($R/sbin/syedge -v 2>&1)，systemd 单元 syedge.service 已启动"

if [[ -n "$SERVER$TOKEN$CAFP" ]]; then
  [[ -n "$SERVER" && -n "$TOKEN" && -n "$CAFP" ]] || die "--server、--token、--ca-fingerprint 需同时提供"
  echo "== 注册到控制台 $SERVER"
  syedge-agent enroll -server "$SERVER" -token "$TOKEN" -ca-fingerprint "$CAFP" ${REPLACE_LOCAL[@]+"${REPLACE_LOCAL[@]}"}
  chown -R syedge-agent:syedge "$STATE_DIR"
  systemctl enable syedge-agent.service && systemctl start syedge-agent.service   # 不用 --now：systemd 220 以下不支持
  echo "syedge-agent 已启动，几秒后节点会出现在控制台。"
elif [[ -f "$STATE_DIR/node.json" ]]; then
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
