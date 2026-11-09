#!/usr/bin/env bash
# 发布包冒烟测试：在一台带 systemd 的干净 Linux 上，用节点发布包走一遍真实安装流程。
#   1. install.sh 安装，syedge 由 systemd 启动，进程名与版本均为 syedge
#   2. 本机启动控制台，导入授权，生成注册令牌
#   3. install.sh --server/--token/--ca-fingerprint 再次执行（验证可重复安装 + 注册）
#   4. 节点上线后发布一个站点，经 sudo + syedge-wrapper 在 systemd 加固环境下完成校验与 reload
#
# 用法（普通用户 + 免密 sudo，CI 中运行）：
#   LIC_TOOL=... LIC_KEY=... LIC_KID=... scripts/release/smoke-node.sh <节点包.tar.gz> <syedge-console>
#   SMOKE_PREFIX=/srv/syedge-smoke SMOKE_UMASK=077 ... scripts/release/smoke-node.sh <节点包.tar.gz> <syedge-console>
# 控制台须是注入了临时授权公钥的测试构建；它只用于本测试，不进入发布包。
set -uo pipefail
PKG=$(realpath "${1:?node package}"); CONSOLE=$(realpath "${2:?console binary}")
: "${LIC_TOOL:?}" "${LIC_KEY:?}" "${LIC_KID:?}"
export NO_PROXY='*' no_proxy='*'
W=$(mktemp -d); JAR=$W/jar
B=http://127.0.0.1:18443/api/v1
ROOT=${SMOKE_PREFIX:-/opt/syedge}
if [[ "$ROOT" == "/opt/syedge" ]]; then
  LOG_DIR=/var/log/syedge
else
  LOG_DIR="$ROOT/logs"
fi
PREFIX_ARGS=()
[[ -z "${SMOKE_PREFIX:-}" ]] || PREFIX_ARGS=(--prefix "$SMOKE_PREFIX")
[[ "${SMOKE_ROOT_MASTER:-}" != 1 ]] || PREFIX_ARGS+=(--root-master)
C=(curl -s -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=smoke-node::${1//$'\n'/%0A}"; fail=$((fail+1)); }
j()   { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
expect() { if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（期望包含 '$3'，实际 '${2:0:300}'）"; fi; }
install_node() {
  if [[ -n "${SMOKE_UMASK:-}" ]]; then
    sudo bash -c 'umask "$1"; shift; exec "$@"' -- "$SMOKE_UMASK" "$D/install.sh" "$@"
  else
    sudo "$D/install.sh" "$@"
  fi
}
cleanup() { [[ -n "${CPID:-}" ]] && kill "$CPID" 2>/dev/null; }
trap cleanup EXIT
diag() {
  [[ -n "${GITHUB_ACTIONS:-}" ]] || return 0
  for u in syedge syedge-agent; do
    m=$( (sudo systemctl status $u --no-pager -l; sudo journalctl -u $u --no-pager -n 20) 2>&1 | tail -30)
    echo "::warning title=$u status::${m//$'\n'/%0A}"
  done
}

echo "== 安装节点包"
tar -C "$W" -xzf "$PKG"
D=$(echo "$W"/syedge-node-*)
if out=$(install_node "${PREFIX_ARGS[@]}" 2>&1); then ok "install.sh 安装成功"; else bad "install.sh 安装失败：$(echo "$out" | tail -12)"; diag; fi
expect "安装根目录" "$(sudo syedge-instance prefix 2>&1)" "$ROOT"
if [[ -x "$ROOT/sbin/syedge" && -e "$ROOT/conf/current" ]]; then ok "自定义根目录文件与配置存在"; else bad "自定义根目录文件或配置缺失：$ROOT"; fi
if [[ -f /etc/logrotate.d/syedge ]]; then ok "日志轮转配置存在"; else bad "日志轮转配置缺失"; fi
expect "systemd 单元 syedge.service 运行中" "$(systemctl is-active syedge.service)" "active"
expect "syedge -v" "$(syedge -v 2>&1)" "SyEdge version: SyEdge/"
expect "master 进程标题" "$(ps -eo args)" "SyEdge: master process"
expect "worker 以 syedge 用户运行" "$(ps -o user= -C syedge | sort -u | tr '\n' ' ')" "syedge"
if ps -eo args | grep -v grep | grep -qi 'ngi''nx'; then bad "进程列表出现上游名"; else ok "进程列表无上游名"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1/ || true)
expect "基线配置：未知域名直接断开" "$code" "000"
MASTER=$(cat /run/syedge/syedge.pid)
# 模拟 logrotate 重命名，验证正在服务的 master 真正重新打开自定义目录中的日志。
if sudo mv "$LOG_DIR/error.log" "$LOG_DIR/error.log.smoke-rotated"; then
  if out=$(sudo syedge-instance reopen-all 2>&1); then
    reopened=0
    for _ in $(seq 1 50); do
      if sudo find "/proc/$MASTER/fd" -maxdepth 1 -type l -lname "$LOG_DIR/error.log" -print -quit | grep -q .; then reopened=1; break; fi
      sleep 0.1
    done
    if [[ "$reopened" == 1 ]]; then ok "日志轮转后 master 重新打开日志"; else bad "日志轮转后 master 未重新打开日志"; fi
  else
    bad "日志轮转后重新打开日志失败：$out"
  fi
else
  bad "日志轮转测试无法重命名错误日志"
fi
expect "节点防火墙默认不启用" "$(sudo syedge-firewall status)" "未启用"
printf 'standalone-static-preserved\n' | sudo tee "$ROOT/html/index.html" >/dev/null
printf '# smoke-logrotate-policy\n' | sudo tee -a /etc/logrotate.d/syedge >/dev/null
before=$(sha256sum /usr/bin/syedge-agent)
if install_node --prefix "${ROOT}-conflict" >/dev/null 2>&1; then
  bad "冲突根目录被接受"
else
  ok "冲突根目录被拒绝"
fi
expect "拒绝冲突时已安装程序不变" "$(sha256sum /usr/bin/syedge-agent)" "$before"

echo "== 启动控制台并导入授权"
"$CONSOLE" -listen 127.0.0.1:18443 -grpc-listen 127.0.0.1:9443 -grpc-hosts 127.0.0.1 \
  -grpc-advertise 127.0.0.1:9443 -data "$W/console" > "$W/console.log" 2>&1 &
CPID=$!
for _ in $(seq 1 60); do curl -sf "$B/setup/status" >/dev/null && break; sleep 1; done
"${C[@]}" -X POST "$B/setup" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
"${C[@]}" -X POST "$B/auth/login" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
INST=$(sed -E 's/.*"installation_id":"([^"]+)".*/\1/' "$W/console/installation.json")
"$LIC_TOOL" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only \
  -customer smoke -days 365 > "$W/smoke.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/smoke.lic" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "授权有效" "$("${C[@]}" "$B/license" | j "d['state']")" "VALID"
if [[ "$ROOT" != /opt/syedge ]]; then
  profile=$(python3 -c 'import json,sys; print(json.dumps({"mode":"system","root":sys.argv[1]}))' "$ROOT")
  result=$("${C[@]}" -X PUT "$B/node-groups/1/profile" -d "$profile")
  expect "节点组使用自定义根目录" "$result" "$ROOT"
fi

echo "== install.sh 带注册参数重复执行"
e=$("${C[@]}" -X POST "$B/node-groups/1/enroll")
srv=$(echo "$e" | j "d['server']"); tok=$(echo "$e" | j "d['token']"); fp=$(echo "$e" | j "d['ca_fingerprint']")
if out=$(install_node "${PREFIX_ARGS[@]}" --server "$srv" --token "$tok" --ca-fingerprint "$fp" 2>&1); then ok "注册安装成功"; else bad "注册安装失败：$(echo "$out" | tail -12)"; diag; fi
expect "重复安装保留静态文件" "$(cat "$ROOT/html/index.html")" "standalone-static-preserved"
expect "重复安装保留轮转策略" "$(cat /etc/logrotate.d/syedge)" "smoke-logrotate-policy"
online=0
for _ in $(seq 1 30); do
  online=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n['status']=='online')")
  [[ "$online" == "1" ]] && break
  sleep 2
done
expect "节点上线" "$online" "1"
expect "systemd 单元 syedge-agent.service 运行中" "$(systemctl is-active syedge-agent.service)" "active"
expect "agent 以 syedge-agent 用户运行" "$(ps -o user= -C syedge-agent)" "syedge-agent"

echo "== 发布站点（sudo + wrapper 在 systemd 加固下校验与 reload）"
"${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"smoke","domains":["smoke.local"],"routes":[{"path":"/","match":"prefix","mode":"static","static":{"index_html":"<h1>smoke ok</h1>"}}]}}' >/dev/null
id=$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{"note":"smoke"}' | j "d.get('id','')")
st=""
for _ in $(seq 1 60); do
  st=$("${C[@]}" "$B/deployments/$id" | j "d['status']" 2>/dev/null)
  [[ -n "$st" && "$st" != "APPLYING" ]] && break
  sleep 1
done
expect "发布生效" "$st" "APPLIED"
expect "站点返回内容" "$(curl -s --max-time 3 -H 'Host: smoke.local' http://127.0.0.1/)" "smoke ok"
expect "Server 头为 SyEdge" "$(curl -sI --max-time 3 -H 'Host: smoke.local' http://127.0.0.1/)" "Server: SyEdge"
expect "发布与升级全程未重启 syedge（master PID 不变）" "$(cat /run/syedge/syedge.pid)" "$MASTER"

if [[ $fail -ne 0 ]]; then
  diag
  tail -15 "$W/console.log" | sed 's/^/::warning title=console::/'
  sudo tail -5 "$LOG_DIR/error.log" | sed 's/^/::warning title=error.log::/'
fi
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "NODE PACKAGE SMOKE OK" || exit 1
