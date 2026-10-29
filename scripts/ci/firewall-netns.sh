#!/usr/bin/env bash
# 节点防火墙的真实收发包测试：两个网络命名空间 + veth，验证 syapp-firewall 的实际效果。
# 用法（root）：scripts/ci/firewall-netns.sh <syapp-agent>
#   节点 10.99.0.1；客户端 10.99.0.2（允许 SSH）与 10.99.0.3（不允许）
set -uo pipefail
AGENT=$(realpath "$1"); FW=$(realpath "$(dirname "$0")/../../dataplane/packaging/firewall/syapp-firewall")
[[ $EUID -eq 0 ]] || { echo "需要 root" >&2; exit 2; }
W=$(mktemp -d); fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=firewall::$1"; fail=$((fail+1)); }
N=(ip netns exec etosfw-node); C=(ip netns exec etosfw-cli)
cleanup() { ip netns pids etosfw-node 2>/dev/null | xargs -r kill 2>/dev/null; ip netns del etosfw-node 2>/dev/null; ip netns del etosfw-cli 2>/dev/null; }
trap cleanup EXIT
cleanup
ip netns add etosfw-node && ip netns add etosfw-cli
ip link add fwn0 type veth peer name fwc0
ip link set fwn0 netns etosfw-node && ip link set fwc0 netns etosfw-cli
"${N[@]}" ip addr add 10.99.0.1/24 dev fwn0; "${N[@]}" ip link set fwn0 up; "${N[@]}" ip link set lo up
"${C[@]}" ip addr add 10.99.0.2/24 dev fwc0; "${C[@]}" ip addr add 10.99.0.3/24 dev fwc0
"${C[@]}" ip link set fwc0 up; "${C[@]}" ip link set lo up
for p in 80 22 9100; do "${N[@]}" python3 -m http.server -b 0.0.0.0 $p >/dev/null 2>&1 & done
"${C[@]}" python3 -m http.server -b 10.99.0.2 8000 >/dev/null 2>&1 &
sleep 1.5
# reach <源地址> <端口>：客户端访问节点
reach() { "${C[@]}" curl -s -o /dev/null --max-time 2 --interface "$1" "http://10.99.0.1:$2/" && echo yes || echo no; }
# systemctl 替身：不碰宿主机的 systemd
printf '#!/bin/sh\ncase "$1" in is-active) exit 3;; is-enabled) echo enabled;; esac\nexit 0\n' > "$W/systemctl"; chmod +x "$W/systemctl"
env_fw=(env -u SSH_CONNECTION SYAPP_AGENT="$AGENT" SYAPP_SYSTEMCTL="$W/systemctl" SYAPP_FW_RULES="$W/firewall.nft")

[[ $(reach 10.99.0.3 9100) == yes ]] && ok "启用前所有端口可达" || bad "启用前 9100 不可达（测试环境问题）"

echo "== 防锁死检查"
out=$("${N[@]}" "${env_fw[@]}" SSH_CONNECTION="10.99.0.3 50000 10.99.0.1 22" "$FW" enable --ssh-from 10.99.0.2 2>&1)
[[ $? -ne 0 && "$out" == *"10.99.0.3"* ]] && ok "当前 SSH 来源不在列表中时拒绝启用" || bad "未阻止锁死：$out"
"${N[@]}" nft list table inet syapp_fw >/dev/null 2>&1 && bad "拒绝时仍加载了规则" || ok "拒绝时未加载规则"
out=$("${N[@]}" "${env_fw[@]}" "$FW" enable 2>&1)
[[ $? -ne 0 ]] && ok "未指定 --ssh-from 时拒绝" || bad "未指定 --ssh-from 仍启用"
out=$("${N[@]}" "${env_fw[@]}" "$FW" enable --ssh-from '10.0.0.0/8;flush ruleset' 2>&1)
[[ $? -ne 0 ]] && ok "注入的来源被拒绝" || bad "注入的来源被接受"

echo "== 启用"
out=$("${N[@]}" "${env_fw[@]}" SSH_CONNECTION="10.99.0.2 50000 10.99.0.1 22" "$FW" enable --ssh-from 10.99.0.2/32 2>&1) \
  && ok "启用成功" || bad "启用失败：$out"
"${N[@]}" "${env_fw[@]}" SSH_CONNECTION="10.99.0.2 50000 10.99.0.1 22" "$FW" enable --ssh-from 10.99.0.2/32 >/dev/null 2>&1
n=$("${N[@]}" nft list table inet syapp_fw | grep -c 'tcp dport 22')
[[ $n == 1 ]] && ok "重复启用不叠加规则" || bad "重复启用后有 $n 条 SSH 规则"
[[ $(stat -c %a "$W/firewall.nft") == 600 ]] && ok "规则文件权限 0600" || bad "规则文件权限 $(stat -c %a "$W/firewall.nft")"
[[ $(reach 10.99.0.3 80) == yes ]] && ok "任意来源可访问 80" || bad "80 被拦截"
[[ $(reach 10.99.0.2 22) == yes ]] && ok "允许的来源可访问 SSH" || bad "允许的来源访问 SSH 被拦截"
[[ $(reach 10.99.0.3 22) == no ]]  && ok "其他来源访问 SSH 被丢弃" || bad "其他来源可以访问 SSH"
[[ $(reach 10.99.0.2 9100) == no ]] && ok "未开放端口被丢弃" || bad "9100 未被拦截"
"${N[@]}" curl -s -o /dev/null --max-time 2 http://10.99.0.2:8000/ && ok "出站不受限（agent 可主动连接控制台）" || bad "出站被拦截"
"${N[@]}" curl -s -o /dev/null --max-time 2 http://127.0.0.1:9100/ && ok "本机回环不受限（状态页 / syapp -t）" || bad "回环被拦截"
"${N[@]}" "${env_fw[@]}" "$FW" status | grep -q "已启用" && ok "status 显示已启用" || bad "status 不正确"

echo "== 追加端口"
"${N[@]}" "${env_fw[@]}" "$FW" enable --ssh-from 10.99.0.2 --allow-tcp 9100 >/dev/null 2>&1
[[ $(reach 10.99.0.3 9100) == yes ]] && ok "--allow-tcp 9100 生效" || bad "--allow-tcp 未生效"
[[ $(reach 10.99.0.3 22) == no ]] && ok "重新生成后 SSH 仍受限" || bad "重新生成后 SSH 放开了"

echo "== 停用"
"${N[@]}" "${env_fw[@]}" "$FW" disable >/dev/null
[[ $(reach 10.99.0.3 22) == yes ]] && ok "停用后规则移除" || bad "停用后仍拦截"
"${N[@]}" "${env_fw[@]}" "$FW" status | grep -q "未启用" && ok "status 显示未启用" || bad "status 不正确"

echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "FIREWALL NETNS OK" || exit 1
