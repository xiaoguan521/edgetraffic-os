#!/usr/bin/env bash
# 规格书 12.3 P9 验收：“控制台单实例宕机业务无感，切换 < 30 秒”。测三种故障下备控制台接管的实际耗时：
#   1. 主机断网 / 断电：主控制台放在独立网络命名空间里，用 iptables 丢弃它的全部流量（不发 FIN / RST，
#      数据库只能靠 TCP 探测发现会话已死）；同时检查旧主先降级、不出现两个主；
#   2. 进程崩溃（SIGKILL）；
#   3. 正常停止（SIGTERM）。
# 接管 = 备控制台 /healthz 返回 leader 且 API 可登录。keepalived 按 docs/ha.md 的参数（interval 2、fall 2、rise 1）
# 漂移 VIP 最多再加 4 秒，所以门槛是接管 ≤ 26 秒。
# 用法（root）：scripts/ci/ha-failover.sh <syedge-console> <syedge-license-server> <PostgreSQL 主机端口，如 127.0.0.1:5432> <用户:密码>
set -uo pipefail
CONSOLE=$(realpath "$1"); LIC=$(realpath "$2"); PGHP=$3; PGUP=$4
: "${LIC_KEY:?}" "${LIC_KID:?}"
[[ $EUID -eq 0 ]] || { echo "需要 root" >&2; exit 2; }
export NO_PROXY='*' no_proxy='*'
W=$(mktemp -d); fail=0; LIMIT=26
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=ha-failover::$1"; fail=$((fail+1)); }
note() { echo "     $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::notice title=ha-failover::$1"; }
j() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
NS=(ip netns exec etosha-a)
PIDS=()
cleanup() { kill "${PIDS[@]}" 2>/dev/null; ip netns pids etosha-a 2>/dev/null | xargs -r kill 2>/dev/null; ip netns del etosha-a 2>/dev/null; ip link del hah0 2>/dev/null; }
trap cleanup EXIT
cleanup

PGPORT=${PGHP##*:}
PGPASSWORD=${PGUP#*:} psql -h "${PGHP%:*}" -p "$PGPORT" -U "${PGUP%%:*}" -q -c 'DROP DATABASE IF EXISTS ha' -c 'CREATE DATABASE ha' || { bad "无法创建测试数据库"; exit 1; }

# 控制台 A 所在的“主机”：独立网络命名空间，经 veth 访问宿主机上的 PostgreSQL（10.250.0.1:$PGPORT）
ip netns add etosha-a
ip link add hah0 type veth peer name haa0
ip link set haa0 netns etosha-a
ip addr add 10.250.0.1/24 dev hah0 && ip link set hah0 up
"${NS[@]}" ip addr add 10.250.0.2/24 dev haa0; "${NS[@]}" ip link set haa0 up; "${NS[@]}" ip link set lo up
"${NS[@]}" ip route add default via 10.250.0.1
DSN_A="postgres://$PGUP@10.250.0.1:$PGPORT/ha?sslmode=disable"
DSN_B="postgres://$PGUP@$PGHP/ha?sslmode=disable"
if ! "${NS[@]}" bash -c "exec 3<>/dev/tcp/10.250.0.1/$PGPORT" 2>/dev/null; then
  # 宿主机上的 PostgreSQL 只监听回环时，用 socat 转发到 veth 地址
  command -v socat >/dev/null || apt-get install -y -qq socat >/dev/null
  socat TCP-LISTEN:"$PGPORT",bind=10.250.0.1,fork,reuseaddr TCP:"$PGHP" & PIDS+=($!)
  sleep 0.5
fi
"${NS[@]}" bash -c "exec 3<>/dev/tcp/10.250.0.1/$PGPORT" 2>/dev/null && ok "命名空间内可访问 PostgreSQL" || { bad "命名空间内无法访问 PostgreSQL"; exit 1; }

HA=https://10.250.0.2:18700; HB=https://127.0.0.1:18701
CA=$W/a/pki/web-ca.crt
hz() { "${@:2}" curl -s --max-time 3 --cacert "$CA" "$1/healthz" 2>/dev/null | j "d.get('role','')" 2>/dev/null; }
start_a() { "${NS[@]}" "$CONSOLE" -listen 10.250.0.2:18700 -grpc-listen 10.250.0.2:19700 -grpc-hosts 10.250.0.2 -web-hosts 10.250.0.2 -data "$W/a" -tls-auto -db "$DSN_A" >> "$W/a.log" 2>&1 & APID=$!; PIDS+=($APID); }
start_b() { "$CONSOLE" -listen 127.0.0.1:18701 -grpc-listen 127.0.0.1:19701 -grpc-hosts 127.0.0.1 -web-hosts 127.0.0.1 -data "$W/b" -tls-auto -db "$DSN_B" >> "$W/b.log" 2>&1 & BPID=$!; PIDS+=($BPID); }
waitrole() { # <url> <role> <秒> [ns 前缀...] -> 0 表示达到
  local u=$1 r=$2 n=$(( $3 * 5 )); shift 3
  for ((i = 0; i < n; i++)); do [[ $(hz "$u" "$@") == "$r" ]] && return 0; sleep 0.2; done
  return 1
}
login() { # <url> [ns 前缀...]
  local u=$1; shift
  "$@" curl -s --max-time 2 --cacert "$CA" -H 'X-Requested-With: etos' -H 'Content-Type: application/json' \
    -X POST "$u/api/v1/auth/login" -d '{"username":"admin","password":"Ha@2026!failover"}' 2>/dev/null
}
now() { date +%s.%N; }
secs() { python3 -c "import sys; print('%.1f' % (float(sys.argv[2]) - float(sys.argv[1])))" "$1" "$2"; }
within() { python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) <= float(sys.argv[2]) else 1)" "$1" "$2"; }

echo "== 准备：A 为主（命名空间内），B 为备（宿主机）"
start_a
for _ in $(seq 1 60); do [[ -f "$CA" ]] && "${NS[@]}" curl -s --cacert "$CA" "$HA/api/v1/setup/status" >/dev/null 2>&1 && break; sleep 0.5; done
JA=$W/jar; CC=("${NS[@]}" curl -s --cacert "$CA" -b "$JA" -c "$JA" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
"${CC[@]}" -X POST "$HA/api/v1/setup" -d '{"username":"admin","password":"Ha@2026!failover"}' >/dev/null
"${CC[@]}" -X POST "$HA/api/v1/auth/login" -d '{"username":"admin","password":"Ha@2026!failover"}' >/dev/null
INST=$(sed -E 's/.*"installation_id":"([^"]+)".*/\1/' "$W/a/installation.json")
"$LIC" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only -customer ha -edition professional -days 365 > "$W/l.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/l.lic" | "${CC[@]}" -X POST "$HA/api/v1/license/import" -d @- >/dev/null
[[ $("${CC[@]}" "$HA/api/v1/license" | j "'ha' in d.get('features', [])") == True ]] && ok "授权包含 ha" || bad "授权不含 ha"
mkdir -p "$W/b" && cp -r "$W/a/pki" "$W/a/secret.key" "$W/b/"
start_b
waitrole "$HA" leader 30 "${NS[@]}" && ok "A 为主" || bad "A 未成为主"
waitrole "$HB" standby 30 && ok "B 为备" || bad "B 未进入备状态：$(tail -3 "$W/b.log")"

echo "== 故障 1：A 所在主机断网（数据包全部丢弃，不发 FIN / RST）"
"${NS[@]}" iptables -A INPUT -i lo -j ACCEPT; "${NS[@]}" iptables -A OUTPUT -o lo -j ACCEPT
t0=$(now)
"${NS[@]}" iptables -A INPUT -j DROP; "${NS[@]}" iptables -A OUTPUT -j DROP
# 两个探测并行进行，各自记录首次满足条件的时间（A 断网后每次探测可能要等数据库超时，不能拖慢对 B 的计时）
( for _ in $(seq 1 300); do [[ $(hz "$HA" "${NS[@]}") == standby ]] && { now > "$W/tA"; break; }; sleep 0.2; done ) &
( for _ in $(seq 1 300); do [[ $(hz "$HB") == leader ]] && { now > "$W/tB"; break; }; sleep 0.2; done ) &
wait $! ; for _ in $(seq 1 100); do [[ -f "$W/tA" ]] && break; sleep 0.2; done
tA=$(cat "$W/tA" 2>/dev/null); tB=$(cat "$W/tB" 2>/dev/null)
if [[ -n "$tB" ]] && login "$HB" | grep -q '"admin"'; then tB2=$(now); d=$(secs "$t0" "$tB2")
  within "$d" $LIMIT && ok "断网后 B 接管并可登录：${d}s ≤ ${LIMIT}s" || bad "断网后接管 ${d}s 超过 ${LIMIT}s"
  note "主机断网：接管 ${d}s（+ keepalived VIP ≤ 4s）"
else bad "断网 60 秒内 B 未接管"; fi
if [[ -n "$tA" ]]; then dA=$(secs "$t0" "$tA"); ok "断网的 A 在 ${dA}s 时自行降级"
  [[ -n "$tB" ]] && { within "$(secs "$tA" "$tB")" 600 && ok "旧主先降级、新主后接管，没有两个主" || bad "出现两个主（B 在 A 降级前接管）"; }
else bad "断网的 A 没有降级"; fi
"${NS[@]}" iptables -F
sleep 4
[[ $(hz "$HA" "${NS[@]}") == standby ]] && ok "网络恢复后 A 作为备，不抢回主" || bad "网络恢复后 A 状态异常：$(hz "$HA" "${NS[@]}")"

echo "== 故障 2：主控制台进程崩溃（SIGKILL）"
t0=$(now); kill -9 "$BPID"; wait "$BPID" 2>/dev/null
if waitrole "$HA" leader 60 "${NS[@]}" && login "$HA" "${NS[@]}" | grep -q '"admin"'; then d=$(secs "$t0" "$(now)")
  within "$d" $LIMIT && ok "进程崩溃后 A 接管并可登录：${d}s ≤ ${LIMIT}s" || bad "进程崩溃后接管 ${d}s 超过 ${LIMIT}s"
  note "进程崩溃：接管 ${d}s"
else bad "进程崩溃 60 秒内 A 未接管"; fi

echo "== 故障 3：主控制台正常停止（SIGTERM）"
start_b
waitrole "$HB" standby 30 || bad "B 重启后未进入备状态"
t0=$(now); kill -TERM "$APID"; wait "$APID" 2>/dev/null
if waitrole "$HB" leader 60 && login "$HB" | grep -q '"admin"'; then d=$(secs "$t0" "$(now)")
  within "$d" $LIMIT && ok "正常停止后 B 接管并可登录：${d}s ≤ ${LIMIT}s" || bad "正常停止后接管 ${d}s 超过 ${LIMIT}s"
  note "正常停止：接管 ${d}s"
else bad "正常停止 60 秒内 B 未接管"; fi

if grep -q "PRIVATE KEY" "$W/a.log" "$W/b.log"; then bad "日志中出现私钥"; fi
echo
if (( fail > 0 )); then echo "主备切换：$fail 项失败"; tail -15 "$W/a.log" "$W/b.log"; exit 1; fi
echo "主备切换：全部通过"
