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
ROOT=${SMOKE_PREFIX:-${SYEDGE_PREFIX:-/opt/syedge}}
DEFAULT_ROOT=${SYEDGE_PREFIX:-/opt/syedge}
LOG_DIR="$ROOT/logs"
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
# install.sh 末尾的安装摘要：标题与“校验配置”行必须存在，且该行的命令在本机（安装所在环境）实际执行通过。
# 命令按空格切分后直接执行（不经 sh -c / eval）。
summary_check() {
  local text=$1 label=$2 line
  local -a cmd=()
  expect "$label 打印安装摘要" "$text" "== 安装完成"
  expect "$label 摘要含校验配置" "$text" "校验配置"
  line=$(printf '%s\n' "$text" | sed -n 's/^ *校验配置 *//p' | head -1)
  read -ra cmd <<< "$line"
  if [[ ${#cmd[@]} -gt 0 && "${cmd[0]}" == sudo ]] && "${cmd[@]}" >"$W/verify.out" 2>&1; then
    ok "$label 摘要中的校验命令执行通过"
  else
    bad "$label 摘要中的校验命令执行失败：${line:-（摘要中没有校验配置行）} $(tail -5 "$W/verify.out" 2>/dev/null)"
  fi
}
cleanup() {
  [[ -n "${CPID:-}" ]] && kill "$CPID" 2>/dev/null
  [[ -z "${HOME_PROBE:-}" ]] || sudo rm -rf "$HOME_PROBE"
  [[ -z "${ROOT_PROBE:-}" ]] || sudo rm -rf "$ROOT_PROBE"
}
trap cleanup EXIT
check_home_sandbox() {
  local pid=$1 name=$2
  if sudo nsenter --target "$pid" --mount -- sh -c 'test ! -e "$1/probe" && test ! -e "$2/probe"' -- "$HOME_PROBE" "$ROOT_PROBE"; then
    ok "$name 隐藏其他家目录"
  else
    bad "$name 家目录隔离无效"
  fi
}
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
if [[ -n "${SMOKE_ROOT_MODE:-}" ]]; then
  sudo install -d -o root -g root -m "$SMOKE_ROOT_MODE" "$ROOT"
fi
if [[ "${SMOKE_HOME_SANDBOX:-}" == 1 ]]; then
  HOME_PROBE=$(sudo mktemp -d /home/syedge-smoke-private.XXXXXX)
  ROOT_PROBE=$(sudo mktemp -d /root/syedge-smoke-private.XXXXXX)
  sudo touch "$HOME_PROBE/probe" "$ROOT_PROBE/probe"
fi
if out=$(install_node "${PREFIX_ARGS[@]}" 2>&1); then ok "install.sh 安装成功"; else bad "install.sh 安装失败：$(echo "$out" | tail -12)"; diag; fi
summary_check "$out" "首次安装"
if [[ -n "${SMOKE_ROOT_MODE:-}" ]]; then
  expect "安装保留已有根目录权限" "$(stat -c %a "$ROOT")" "${SMOKE_ROOT_MODE#0}"
fi
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
MASTER=$(cat $ROOT/run/syedge.pid)
if [[ "${SMOKE_HOME_SANDBOX:-}" == 1 ]]; then
  expect "数据面 home 沙箱" "$(systemctl show syedge.service -p ProtectHome --value)" "tmpfs"
  check_home_sandbox "$MASTER" "数据面"
fi
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
echo "== 创建独立实例"
SOLO_PID=""
if out=$(sudo syedge-instance add solo --http 28080 --https 28443 --status 28081 --resource-mode independent 2>&1); then
  ok "独立实例创建成功"
  expect "独立实例 systemd 服务运行" "$(systemctl is-active syedge@solo.service)" "active"
  expect "独立实例资源列表" "$(sudo syedge-instance runtimes)" "$(printf 'solo\tindependent\t%s/instances/solo' "$ROOT")"
  SOLO_PID=$(cat $ROOT/instances/solo/run/syedge.pid 2>/dev/null || true)
  expect "独立实例使用自己的程序" "$(sudo readlink "/proc/$SOLO_PID/exe" 2>/dev/null)" "$ROOT/instances/solo/sbin/syedge"
  if sudo syedge-instance reopen-all >/dev/null 2>&1; then ok "共享及独立实例日志轮转"; else bad "独立实例日志重新打开失败"; fi
else
  bad "独立实例创建失败：$(echo "$out" | tail -12)"
fi
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
if [[ "$ROOT" != "$DEFAULT_ROOT" ]]; then
  profile=$(python3 -c 'import json,sys; print(json.dumps({"mode":"system","root":sys.argv[1]}))' "$ROOT")
  result=$("${C[@]}" -X PUT "$B/node-groups/1/profile" -d "$profile")
  expect "节点组使用自定义根目录" "$result" "$ROOT"
fi

echo "== install.sh 带注册参数重复执行"
e=$("${C[@]}" -X POST "$B/node-groups/1/enroll")
srv=$(echo "$e" | j "d['server']"); tok=$(echo "$e" | j "d['token']"); fp=$(echo "$e" | j "d['ca_fingerprint']")
if out=$(install_node "${PREFIX_ARGS[@]}" --server "$srv" --token "$tok" --ca-fingerprint "$fp" 2>&1); then ok "注册安装成功"; else bad "注册安装失败：$(echo "$out" | tail -12)"; diag; fi
summary_check "$out" "注册安装"
if [[ -n "${SMOKE_ROOT_MODE:-}" ]]; then
  expect "重复安装保留根目录权限" "$(stat -c %a "$ROOT")" "${SMOKE_ROOT_MODE#0}"
fi
expect "重复安装保留静态文件" "$(cat "$ROOT/html/index.html")" "standalone-static-preserved"
expect "重复安装保留轮转策略" "$(cat /etc/logrotate.d/syedge)" "smoke-logrotate-policy"
if [[ -n "$SOLO_PID" ]]; then
  expect "重复安装保留独立实例" "$(cat $ROOT/instances/solo/run/syedge.pid 2>/dev/null)" "$SOLO_PID"
  expect "独立实例状态页" "$(curl -s --max-time 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:28081/__syedge_status)" "200"
fi
online=0
for _ in $(seq 1 30); do
  online=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n['status']=='online')")
  [[ "$online" == "1" ]] && break
  sleep 2
done
expect "节点上线" "$online" "1"
expect "systemd 单元 syedge-agent.service 运行中" "$(systemctl is-active syedge-agent.service)" "active"
expect "agent 以 syedge-agent 用户运行" "$(ps -o user= -C syedge-agent)" "syedge-agent"
if [[ "${SMOKE_HOME_SANDBOX:-}" == 1 ]]; then
  expect "agent home 沙箱" "$(systemctl show syedge-agent.service -p ProtectHome --value)" "tmpfs"
  check_home_sandbox "$(systemctl show syedge-agent.service -p MainPID --value)" "agent"
fi

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
expect "发布与升级全程未重启 syedge（master PID 不变）" "$(cat $ROOT/run/syedge.pid)" "$MASTER"
if [[ -n "$SOLO_PID" ]]; then
  if [[ "$ROOT" != /opt/syedge ]]; then
    printf 'preserved\n' | sudo tee "$ROOT/instances/solo/logs/preserve-smoke.log" "$ROOT/instances/solo/cache/preserve-smoke.cache" >/dev/null
  fi
  if out=$(sudo syedge-instance remove solo --yes 2>&1); then ok "独立实例可单独删除"; else bad "独立实例删除失败：$out"; fi
  if [[ "$ROOT" != /opt/syedge ]]; then
    if sudo find "$ROOT/removed-instances" -name preserve-smoke.log -print -quit | grep -q . && sudo find "$ROOT/removed-instances" -name preserve-smoke.cache -print -quit | grep -q .; then
      ok "独立实例删除时保留日志与缓存"
    else
      bad "独立实例删除时未保留日志或缓存"
    fi
  fi
  expect "删除独立实例不影响默认实例" "$(cat $ROOT/run/syedge.pid)" "$MASTER"
fi

echo "== systemd 优雅停止时保留正在传输的连接"
printf 'server { listen 127.0.0.1:18081; location / { root %s/html; limit_rate 2k; } }\n' "$ROOT" |
  sudo tee "$ROOT/conf/current/conf.d/graceful-stop.conf" >/dev/null
sudo dd if=/dev/zero of="$ROOT/html/graceful-stop.bin" bs=1024 count=16 status=none
if out=$(sudo systemctl reload syedge.service 2>&1); then
  # reload 是异步的；先等待新监听端口就绪，避免把启动竞争误判为断连。
  for _ in $(seq 1 100); do
    if curl -fsSI --max-time 1 http://127.0.0.1:18081/graceful-stop.bin >/dev/null 2>&1; then break; fi
    sleep 0.1
  done
  curl -fsS --max-time 30 -o "$W/graceful-response" http://127.0.0.1:18081/graceful-stop.bin &
  slow_pid=$!
  started=0
  for _ in $(seq 1 100); do
    if [[ -s "$W/graceful-response" ]]; then started=1; break; fi
    kill -0 "$slow_pid" 2>/dev/null || break
    sleep 0.05
  done
  if [[ "$started" == 1 ]]; then
    if ! sudo systemctl stop syedge.service; then bad "systemd 停止失败"; fi
    if wait "$slow_pid" && [[ $(wc -c < "$W/graceful-response") -eq 16384 ]]; then
      ok "停止期间慢连接完整传输"
    else
      bad "停止期间慢连接中断"
    fi
  else
    bad "优雅停止测试的慢连接未启动"
    kill "$slow_pid" 2>/dev/null || true
    wait "$slow_pid" 2>/dev/null || true
  fi
else
  bad "优雅停止测试无法加载配置：$out"
fi

if [[ $fail -eq 0 ]]; then
  sudo install -d "$ROOT/data"
  printf 'console-data-preserved\n' | sudo tee "$ROOT/data/purge-smoke" >/dev/null
  if sudo "$D/uninstall.sh" --purge && sudo test -f "$ROOT/data/purge-smoke" && ! sudo test -e "$ROOT/conf"; then
    ok "卸载只删除节点数据，保留同目录下的控制台数据"
  else
    bad "节点卸载没有正确保护同目录下的其他数据"
  fi
fi

if [[ $fail -ne 0 ]]; then
  diag
  tail -15 "$W/console.log" | sed 's/^/::warning title=console::/'
  sudo tail -5 "$LOG_DIR/error.log" | sed 's/^/::warning title=error.log::/'
fi
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "NODE PACKAGE SMOKE OK" || exit 1
