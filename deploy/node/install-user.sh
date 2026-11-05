#!/usr/bin/env bash
# user 模式安装（不需要 root，设计 0001 第 6 节）：把程序复制到安装根目录并创建默认实例。
#
#   ./install-user.sh <安装根目录> --http <端口> --https <端口> --status <端口> \
#       [--console <控制台>:9443 --token <令牌> --ca-fingerprint <指纹>]
#
# 端口须 ≥ 1024（普通用户不能监听特权端口）；同一根目录下再加实例：
#   <安装根目录>/bin/syapp-instance --root <安装根目录> add <实例名> --http .. --https .. --status ..
set -euo pipefail
B=syapp
[[ $# -ge 1 && "$1" != -* ]] || { sed -n '4,8p' "$0" >&2; exit 2; }
R="$1"; shift
[[ "$(id -u)" != 0 ]] || { echo "user 模式请以普通用户运行；以 root 安装请使用 install.sh 或软件包" >&2; exit 1; }
case "$R" in /*) ;; *) echo "安装根目录必须是绝对路径" >&2; exit 2 ;; esac
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$R/bin" "$R/sbin" "$R/modules"
install -m 0755 "$HERE/bin/$B-agent" "$HERE/bin/$B-wrapper" "$HERE/bin/$B-instance" "$R/bin/"
install -m 0755 "$HERE/sbin/$B" "$R/sbin/$B"
if compgen -G "$HERE/modules/*" >/dev/null; then cp -p "$HERE"/modules/* "$R/modules/"; fi
exec "$R/bin/$B-instance" --root "$R" init "$@"
