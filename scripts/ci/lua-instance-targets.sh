#!/usr/bin/env bash
# 在一次性 CI 主机上验证 Lua 安装目标选择、共享范围和独立实例卸载保护。
set -euo pipefail
trap 'rc=$?; echo "lua-instance-targets: line $LINENO failed (exit $rc): $BASH_COMMAND" >&2' ERR
DIST=$(realpath "$1"); BASE=$2; ARCH=$(uname -m)
ROOT=/home/atwasoft/node
[[ ! -e /home/atwasoft && ! -L /home/atwasoft && ! -e /etc/syedge/prefix && ! -L /etc/syedge/prefix && ! -e /usr/sbin/syedge-instance && ! -L /usr/sbin/syedge-instance ]] || { echo '需要干净的 CI 主机' >&2; exit 1; }
W=$(mktemp -d); chmod 0755 "$W"
cleanup() { sudo rm -rf /home/atwasoft /etc/syedge/prefix /usr/sbin/syedge-instance; rm -rf "$W"; }
trap cleanup EXIT
getent group syedge >/dev/null || sudo groupadd -r syedge
id syedge >/dev/null 2>&1 || sudo useradd -r -g syedge -s /usr/sbin/nologin syedge
go build -mod=vendor -o "$W/syedge-instance" ./cmd/syedge-instance
sudo install -m 0755 "$W/syedge-instance" /usr/sbin/syedge-instance
sudo install -d -m 0755 "$ROOT/sbin" "$ROOT/instances/blue/conf/current" "$ROOT/instances/green/sbin" "$ROOT/instances/green/conf/current"
sudo /usr/sbin/syedge-instance prefix "$ROOT"
stdpkg=$(ls "$DIST"/base/syedge-*-"$ARCH".tar.gz | grep -v gmssl | head -1)
gmpkg=$(ls "$DIST"/base/syedge-*-gmssl-"$ARCH".tar.gz | head -1)
sharedpkg=$stdpkg; independentpkg=$gmpkg; independentbase=gmssl
if [[ "$BASE" == gmssl ]]; then sharedpkg=$gmpkg; independentpkg=$stdpkg; independentbase=standard; fi
mkdir "$W/shared" "$W/independent"
tar -C "$W/shared" -xzf "$sharedpkg"
tar -C "$W/independent" -xzf "$independentpkg"
sudo install -m 0755 "$W/shared/opt/syedge/sbin/syedge" "$ROOT/sbin/syedge"
sudo install -m 0755 "$W/independent/opt/syedge/sbin/syedge" "$ROOT/instances/green/sbin/syedge"
printf '{"instance":"blue","mode":"system","root":"%s","http_port":18080,"https_port":18443,"status_port":8082}\n' "$ROOT" | sudo tee "$ROOT/instances/blue/instance.json" >/dev/null
printf '{"instance":"green","mode":"system","root":"%s","resource_mode":"independent","http_port":28080,"https_port":28443,"status_port":8088}\n' "$ROOT" | sudo tee "$ROOT/instances/green/instance.json" >/dev/null
sudo chmod 0644 "$ROOT/instances/blue/instance.json" "$ROOT/instances/green/instance.json"
mkdir "$W/lua"
tar -C "$W/lua" -xzf "$DIST"/syedge-lua-*-"$ARCH".tar.gz
if out=$(sudo bash "$W/lua/install.sh" </dev/null 2>&1); then echo '多个实例时没有要求选择' >&2; exit 1; fi
[[ "$out" == *'--instance'* ]]
# 用伪终端演练真正的交互选择，第三项为独立实例 green。
python3 - "$W/lua/install.sh" <<'PY'
import os, pty, subprocess, sys
master, slave = pty.openpty()
p = subprocess.Popen(["sudo", "bash", sys.argv[1]], stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
os.close(slave)
os.write(master, b"3\n")
try:
    out, _ = p.communicate(timeout=45)
finally:
    os.close(master)
if p.returncode != 0 or "已选择实例：green" not in out:
    raise SystemExit("交互选择失败：" + out)
PY
sudo bash "$W/lua/install.sh" --instance green
[[ -L "$ROOT/instances/green/modules/lua" && ! -e "$ROOT/modules/lua" ]]
[[ $(cat "$ROOT/instances/green/modules/lua/BASE") == "$independentbase" ]]
echo "load_module $ROOT/instances/green/modules/lua/ndk_http_module.so;" | sudo tee "$ROOT/instances/green/conf/current/syedge.conf" >/dev/null
if sudo bash "$W/lua/uninstall.sh" --instance green >/dev/null 2>&1; then echo '独立实例正在使用 Lua，但卸载未被拒绝' >&2; exit 1; fi
sudo bash "$W/lua/install.sh" --instance blue
[[ -L "$ROOT/modules/lua" && -L "$ROOT/instances/green/modules/lua" ]]
[[ $(cat "$ROOT/modules/lua/BASE") == "$BASE" ]]
sudo bash "$W/lua/uninstall.sh" --instance blue
[[ ! -e "$ROOT/modules/lua" && -L "$ROOT/instances/green/modules/lua" ]]
sudo rm "$ROOT/instances/green/conf/current/syedge.conf"
sudo bash "$W/lua/uninstall.sh" --instance green
[[ ! -e "$ROOT/instances/green/modules/lua" ]]
echo "Lua $BASE 目标选择、共享与独立实例保护：OK"
