#!/usr/bin/env bash
# 在一次性 CI 主机上验证 system 自定义根目录的自动识别与多实例卸载保护。
set -euo pipefail
trap 'rc=$?; echo "lua-custom-prefix: line $LINENO failed (exit $rc): $BASH_COMMAND" >&2' ERR
DIST=$(realpath "$1"); BASE=$2; ARCH=$(uname -m)
PREFIX=${SYEDGE_PREFIX:-/opt/syedge}
ROOT=/home/atwasoft/node
[[ "$PREFIX" != "$ROOT" ]] || ROOT=/home/atwasoft/node-ci
[[ ! -e /home/atwasoft && ! -L /home/atwasoft && ! -e /etc/syedge/prefix && ! -L /etc/syedge/prefix && ! -e /usr/sbin/syedge-instance && ! -L /usr/sbin/syedge-instance ]] || { echo '需要干净的 CI 主机' >&2; exit 1; }
W=$(mktemp -d); chmod 0755 "$W"
cleanup() {
  sudo rm -rf /home/atwasoft /etc/syedge/prefix /usr/sbin/syedge-instance
  rm -rf "$W"
}
trap cleanup EXIT
getent group syedge >/dev/null || sudo groupadd -r syedge
id syedge >/dev/null 2>&1 || sudo useradd -r -g syedge -s /usr/sbin/nologin syedge
go build -mod=vendor -ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/layout.defaultSystemRoot=$PREFIX" -o "$W/syedge-instance" ./cmd/syedge-instance
sudo install -m 0755 "$W/syedge-instance" /usr/sbin/syedge-instance
sudo install -d -m 0755 "$ROOT/sbin"
sudo /usr/sbin/syedge-instance prefix "$ROOT"
basepkg=$(ls "$DIST"/base/syedge-*-"$ARCH".tar.gz | grep -v gmssl | head -1)
[[ "$BASE" == gmssl ]] && basepkg=$(ls "$DIST"/base/syedge-*-gmssl-"$ARCH".tar.gz | head -1)
tar -C "$W" -xzf "$basepkg"
sudo install -m 0755 "$W$PREFIX/sbin/syedge" "$ROOT/sbin/syedge"
mkdir "$W/lua"
tar -C "$W/lua" -xzf "$DIST"/syedge-lua-*-"$ARCH".tar.gz
out=$(sudo bash "$W/lua/install.sh" 2>&1) || { echo "$out" >&2; exit 1; }
echo "$out"
[[ "$out" == *"安装根目录：$ROOT"* && "$out" == *"已安装（$BASE）"* ]] || { echo '安装输出与目标或变体不一致' >&2; exit 1; }
[[ -L "$ROOT/modules/lua" && ! -e "$PREFIX/modules/lua" ]] || { echo '模块链接不在目标目录' >&2; exit 1; }
sudo install -d -m 0755 "$ROOT/instances/blue/conf/current"
echo "load_module $ROOT/modules/lua/ndk_http_module.so;" | sudo tee "$ROOT/instances/blue/conf/current/syedge.conf" >/dev/null
if out=$(sudo bash "$W/lua/uninstall.sh" 2>&1); then
  echo '具名实例正在使用 Lua，但卸载未被拒绝' >&2; exit 1
fi
[[ "$out" == *'当前配置正在使用 Lua 规则'* ]] || { echo "卸载失败原因不符合预期：$out" >&2; exit 1; }
sudo rm "$ROOT/instances/blue/conf/current/syedge.conf"
sudo bash "$W/lua/uninstall.sh"
[[ ! -e "$ROOT/modules/lua" ]]
echo "Lua $BASE 自动识别 $ROOT，并保护具名实例：OK"
