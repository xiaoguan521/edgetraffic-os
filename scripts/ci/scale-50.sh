#!/usr/bin/env bash
# 50 节点规模验收（真实容器，规格书 12.1“集成与压测”、12.6 数据面统一升级）：
#   1. 50 节点并行发布 < 60 秒；
#   2. 控制台宕机 30 分钟期间业务无影响（节点持续服务、0 失败请求、syapp 不重启），恢复后节点自动重连、发布正常；
#   3. 50 节点分 3 批热升级 < 10 分钟，回退同样 < 10 分钟；
#   4. 升级与回退都在压测中进行：0 失败请求，长连接不断。
# 在 demo/ 的 Docker 环境中运行（真实控制台 + 50 个真实节点容器：syapp + agent + wrapper）。
# 用法：在仓库根目录 scripts/ci/scale-50.sh；需要 docker、wrk、python3、curl；
#   LIC_TOOL / LIC_KEY / LIC_KID：CI 临时授权签发；PKG_KEY / PKG_KID：CI 临时升级包签名密钥；
#   N（默认 50）、DOWN_MIN（默认 30）可调小用于本地试跑。
set -uo pipefail
cd "$(dirname "$0")/../../demo"
: "${LIC_TOOL:?}" "${LIC_KEY:?}" "${LIC_KID:?}" "${PKG_KEY:?}" "${PKG_KID:?}"
N=${N:-50}; DOWN_MIN=${DOWN_MIN:-30}
DC="docker compose"; NET=edgetraffic-demo_edge; IMG=${ETOS_REGISTRY:-edgetraffic-os}/node:${ETOS_TAG:-demo}
B="http://127.0.0.1:${CONSOLE_PORT:-8443}/api/v1"
W=$(mktemp -d); JAR=$W/jar
C=(curl -s --noproxy '*' -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
HOSTH='Host: shop.scale.local'
fail=0
ok()   { echo "ok   $1"; }
bad()  { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=scale-$N::$1"; fail=$((fail+1)); }
note() { echo "     $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::notice title=scale-$N::$1"; }
j()    { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
now()  { date +%s.%N; }
secs() { python3 -c "import sys; print('%.1f' % (float(sys.argv[2]) - float(sys.argv[1])))" "$1" "$2"; }
le()   { python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) <= float(sys.argv[2]) else 1)" "$1" "$2"; }
ip_of() { echo "172.30.0.$((100 + 10#$1))"; }   # 节点 NN（01..50）的地址
name_of() { printf 'edge-%02d' "$1"; }
PIDS=()
cleanup() {
  kill "${PIDS[@]}" 2>/dev/null
  for i in $(seq 1 "$N"); do docker rm -f "etos-n$i" >/dev/null 2>&1 & done; wait
}
trap cleanup EXIT

echo "== 控制台就绪、初始化、授权（$N 节点）"
for _ in $(seq 1 90); do curl -sf --noproxy '*' "$B/setup/status" >/dev/null && break; sleep 2; done
"${C[@]}" -X POST "$B/setup" -d '{"username":"admin","password":"Scale@2026!50"}' >/dev/null
"${C[@]}" -X POST "$B/auth/login" -d '{"username":"admin","password":"Scale@2026!50"}' >/dev/null
INST=$($DC exec -T console sh -c 'cat /var/lib/syapp-console/installation.json' | sed -E 's/.*"installation_id":"([^"]+)".*/\1/')
"$LIC_TOOL" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only \
  -customer scale -edition professional -nodes $((N + 10)) -sites 200 -days 365 > "$W/l.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/l.lic" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
[[ $("${C[@]}" "$B/license" | j "d['state']") == VALID ]] && ok "授权有效" || { bad "授权导入失败"; exit 1; }

echo "== 启动并注册 $N 个节点"
start_node() {
  local i=$1 nn; nn=$(printf '%02d' "$i")
  docker run -d --name "etos-n$i" --hostname "$(name_of "$i")" --network "$NET" --ip "$(ip_of "$nn")" \
    --ulimit nofile=65535:65535 "$IMG" >/dev/null || return 1
  local cmd; cmd=$("${C[@]}" -X POST "$B/node-groups/1/enroll" | j "d['command']")
  cmd=${cmd#sudo }; cmd=${cmd%% && *}
  for _ in $(seq 1 30); do docker exec "etos-n$i" test -e /opt/syapp/conf/current && break; sleep 1; done
  # shellcheck disable=SC2086
  docker exec "etos-n$i" $cmd >/dev/null 2>&1
}
t0=$(now)
for i in $(seq 1 "$N"); do
  start_node "$i" &
  (( i % 10 == 0 )) && wait
done
wait
online=0
for _ in $(seq 1 120); do
  online=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n['status']=='online')")
  [[ "$online" == "$N" ]] && break; sleep 2
done
[[ "$online" == "$N" ]] && ok "$N 个节点全部在线（$(secs "$t0" "$(now)")s）" || { bad "只有 $online/$N 个节点在线"; exit 1; }

echo "== 1. $N 节点并行发布 < 60 秒"
"${C[@]}" -X POST "$B/upstreams" -d '{"group_id":1,"spec":{"name":"api","servers":[{"addr":"172.30.0.31:9001"},{"addr":"172.30.0.32:9001"}]}}' >/dev/null
"${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"shop","domains":["shop.scale.local"],"routes":[{"path":"/","match":"prefix","mode":"static","static":{"index_html":"scale v1"}},{"path":"/api/","match":"prefix","mode":"proxy","proxy":{"upstream":"api"}},{"path":"/stream","match":"prefix","mode":"proxy","proxy":{"upstream":"api"}}]}}' >/dev/null
publish() { # <备注> -> “耗时 状态 APPLIED 节点数”
  local t id st="" d
  t=$(now)
  id=$("${C[@]}" -X POST "$B/node-groups/1/publish" -d "{\"note\":\"$1\"}" | j "d.get('id','')")
  [[ -z "$id" ]] && { echo "0 REFUSED 0"; return; }
  for _ in $(seq 1 600); do
    d=$("${C[@]}" "$B/deployments/$id"); st=$(j "d['status']" <<<"$d")
    [[ "$st" != "APPLYING" ]] && break; sleep 0.2
  done
  echo "$(secs "$t" "$(now)") $st $(j "sum(1 for n in d['nodes'] if n['status']=='APPLIED')" <<<"$d")"
}
check_publish() { # <说明> <备注>
  local r el st n; r=$(publish "$2"); read -r el st n <<<"$r"
  if [[ "$st" == APPLIED && "$n" == "$N" ]] && le "$el" 60; then ok "$1：$N 节点全部生效，${el}s < 60s"; else bad "$1：${el}s，状态 $st，生效 $n/$N"; fi
  note "$1：${el}s"
}
check_publish "首次发布" "scale v1"
sid=$("${C[@]}" "$B/sites?group_id=1" | j "d[0]['id']")
edit_site() { # <首页文字>
  "${C[@]}" "$B/sites/$sid" | python3 -c "import json,sys;d=json.load(sys.stdin);s=d['spec'];s['routes'][0]['static']['index_html']=sys.argv[1];print(json.dumps({'spec':s,'version':d['version']}))" "$1" \
    | "${C[@]}" -X PUT "$B/sites/$sid" -d @- >/dev/null
}
edit_site "scale v2"
check_publish "修改后再发布" "scale v2"
bodies=$(for i in $(seq 1 "$N"); do curl -s --noproxy '*' --max-time 3 -H "$HOSTH" "http://$(ip_of "$i")/" & done; wait)
[[ $(grep -c "scale v2" <<<"$bodies") == "$N" ]] && ok "$N 个节点都返回新内容" || bad "只有 $(grep -c "scale v2" <<<"$bodies")/$N 个节点返回新内容"

# 业务探测：轮询全部节点（每个请求新建连接），记录成功与失败次数，直到 $W/stop-<名> 出现
probe() { # <名>
  local okc=0 bad=0 i code
  while [[ ! -e "$W/stop-$1" ]]; do
    for i in $(seq 1 "$N"); do
      code=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' --max-time 3 -H "$HOSTH" "http://$(ip_of "$i")/api/probe")
      if [[ "$code" == 200 ]]; then okc=$((okc + 1)); else bad=$((bad + 1)); echo "$(date +%T) $(name_of "$i") $code" >> "$W/probe-$1.err"; fi
    done
    echo "$okc $bad" > "$W/probe-$1"
    sleep 0.5
  done
}
probe_result() { # <名> -> “成功 失败”
  touch "$W/stop-$1"; sleep 4; cat "$W/probe-$1" 2>/dev/null || echo "0 0"
}
masters() { for i in $(seq 1 "$N"); do docker exec "etos-n$i" cat /run/syapp/syapp.pid 2>/dev/null || echo "-"; done | tr '\n' ' '; }

echo "== 2. 控制台宕机 $DOWN_MIN 分钟期间业务无影响"
m_before=$(masters)
probe down & PIDS+=($!)
$DC stop console >/dev/null 2>&1
[[ -z "$(curl -s --noproxy '*' --max-time 2 "$B/setup/status")" ]] && ok "控制台已停止" || bad "控制台仍在响应"
for ((m = 1; m <= DOWN_MIN; m++)); do
  sleep 60
  (( m % 5 == 0 || m == DOWN_MIN )) && echo "     宕机 ${m} 分钟：探测（成功 失败）$(cat "$W/probe-down" 2>/dev/null)"
done
read -r pok pbad <<<"$(probe_result down)"
(( pbad == 0 && pok > N * 100 )) && ok "宕机 $DOWN_MIN 分钟内 $pok 次业务请求全部成功" || bad "宕机期间业务请求 成功 $pok / 失败 $pbad（$(head -3 "$W/probe-down.err" 2>/dev/null | tr '\n' ' ')）"
note "控制台宕机 $DOWN_MIN 分钟：业务请求成功 $pok、失败 $pbad"
[[ "$(masters)" == "$m_before" ]] && ok "宕机期间所有节点的 syapp master 未重启" || bad "有节点的 syapp master 发生变化"
t0=$(now)
$DC start console >/dev/null 2>&1
for _ in $(seq 1 90); do curl -sf --noproxy '*' "$B/setup/status" >/dev/null && break; sleep 1; done
rm -f "$JAR"; "${C[@]}" -X POST "$B/auth/login" -d '{"username":"admin","password":"Scale@2026!50"}' >/dev/null
online=0
for _ in $(seq 1 120); do
  online=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n['status']=='online')")
  [[ "$online" == "$N" ]] && break; sleep 2
done
[[ "$online" == "$N" ]] && ok "控制台恢复后 $N 个节点自动重连（$(secs "$t0" "$(now)")s）" || bad "恢复后只有 $online/$N 个节点重连"
dr=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n.get('drifted'))")
[[ "$dr" == 0 ]] && ok "没有节点报告配置漂移" || bad "$dr 个节点报告配置漂移"
edit_site "scale v3"
check_publish "控制台恢复后发布" "scale v3"

echo "== 3/4. 分 3 批热升级与回退（压测中，0 失败请求、长连接不断）"
VER=$(docker exec etos-n1 /opt/syapp/sbin/syapp -v 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
UP="$VER-2"
docker cp "etos-n1:$(docker exec etos-n1 readlink -f /opt/syapp/sbin/syapp)" "$W/syapp.bin" >/dev/null
ARCH=$(docker exec etos-n1 sh -c 'dpkg --print-architecture 2>/dev/null || uname -m')
mkpkg() { # <版本>：同一个二进制以给定版本号打包并签名，导入版本库
  local v=$1 d="$W/pkg-$1"
  mkdir -p "$d/s/sbin" && cp "$W/syapp.bin" "$d/s/sbin/syapp" && chmod 755 "$d/s/sbin/syapp"
  echo "$v" > "$d/s/VERSION"; cp ../docs/NOTICE "$d/s/NOTICE"
  tar -C "$d/s" --owner=0 --group=0 -czf "$d/syapp-$v.tar.gz" sbin/syapp VERSION NOTICE
  "$LIC_TOOL" sign-package -key "$PKG_KEY" -kid "$PKG_KID" -in "$d/syapp-$v.tar.gz" -name syapp -version "$v" -arch "$ARCH" -variant standard >/dev/null
  curl -s --noproxy '*' -b "$JAR" -H 'X-Requested-With: etos' -F "package=@$d/syapp-$v.tar.gz" -F "signature=@$d/syapp-$v.tar.gz.sig" "$B/packages"
}
expect_pkg() { if [[ "$2" == *'"version":"'"$1"'"'* ]]; then ok "导入签名升级包 syapp $1"; else bad "导入 syapp $1 失败：${2:0:200}"; fi; }
expect_pkg "$UP" "$(mkpkg "$UP")"
expect_pkg "$VER" "$(mkpkg "$VER")"

# 节点按 ID 升序分批：第 1 批 1 台、第 2 批约 10%、第 3 批其余。压测与长连接覆盖每一批中的一台
ids=$("${C[@]}" "$B/nodes" | j "' '.join(n['name'] for n in sorted(d, key=lambda n: n['id']))")
read -r -a order <<<"$ids"
pick=("${order[0]}" "${order[1]}" "${order[$((N - 1))]}")
num() { echo "${1#edge-}"; }

rollout() { # <说明> <目标版本> <期望软链目录>
  local what=$1 ver=$2 n w s
  rm -f "$W"/stop-* "$W"/probe-*
  probe "$ver" & local ppid=$!; PIDS+=($ppid)
  local wrks=() streams=()
  for n in "${pick[@]}"; do
    wrk -t1 -c8 -d1200s -H "$HOSTH" "http://$(ip_of "$(num "$n")")/api/load" > "$W/wrk-$ver-$n.txt" 2>&1 & wrks+=($!); PIDS+=($!)
    curl -sN --noproxy '*' -H "$HOSTH" "http://$(ip_of "$(num "$n")")/stream?seconds=150" > "$W/stream-$ver-$n.txt" 2>"$W/stream-$ver-$n.err" & streams+=($!); PIDS+=($!)
  done
  sleep 3
  local t0 up id st="" v
  t0=$(now)
  up=$("${C[@]}" -X POST "$B/node-groups/1/upgrade" -d "{\"version\":\"$ver\",\"variant\":\"standard\"}")
  id=$(j "d.get('id','')" <<<"$up" 2>/dev/null)
  [[ -z "$id" ]] && { bad "$what：无法开始（${up:0:200}）"; touch "$W/stop-$ver"; kill -INT "${wrks[@]}" "${streams[@]}" 2>/dev/null; return; }
  declare -A done_at=()
  for _ in $(seq 1 3000); do
    v=$("${C[@]}" "$B/upgrades/$id"); st=$(j "d['status']" <<<"$v")
    for n in "${pick[@]}"; do
      [[ -z "${done_at[$n]:-}" && $(j "[x['status'] for x in d['nodes'] if x['name']=='$n'][0]" <<<"$v") == upgraded ]] && done_at[$n]=$(now)
    done
    [[ "$st" != running ]] && break; sleep 0.2
  done
  local el; el=$(secs "$t0" "$(now)")
  local nup; nup=$(j "sum(1 for x in d['nodes'] if x['status']=='upgraded')" <<<"$v")
  local batches; batches=$(j "' / '.join(str(sum(1 for x in d['nodes'] if x['batch']==b)) for b in sorted(set(x['batch'] for x in d['nodes'])))" <<<"$v")
  if [[ "$st" == done && "$nup" == "$N" ]] && le "$el" 600; then ok "$what：$N 节点分批（$batches）全部完成，${el}s < 600s"
  else bad "$what：状态 $st，完成 $nup/$N，${el}s（$(j "[(x['name'], x['status'], x.get('message','')) for x in d['nodes'] if x['status']!='upgraded'][:3]" <<<"$v")）"; fi
  note "$what：${el}s，批次 $batches"
  # 长连接：在本节点升级完成后才结束，且完整收到全部 150 行
  for i in "${!streams[@]}"; do
    n=${pick[$i]}
    wait "${streams[$i]}"; s=$?
    local lines; lines=$(grep -c "line " "$W/stream-$ver-$n.txt")
    local end; end=$(stat -c %Y "$W/stream-$ver-$n.txt")
    if [[ $s -eq 0 && "$lines" == 150 ]]; then
      if [[ -n "${done_at[$n]:-}" ]] && le "${done_at[$n]}" "$end.999"; then ok "$what：$n 的长连接跨越了该节点的升级，完整收到 150 行"
      else bad "$what：$n 的长连接在该节点升级完成前就结束了（测试未覆盖）"; fi
    else bad "$what：$n 的长连接中断（curl 退出码 $s，收到 $lines/150 行）"; fi
  done
  kill -INT "${wrks[@]}" 2>/dev/null; wait "${wrks[@]}" 2>/dev/null
  for n in "${pick[@]}"; do
    local f="$W/wrk-$ver-$n.txt" req errs non
    req=$(awk '/requests in/ {print $1}' "$f")
    errs=$(awk '/Socket errors/ {gsub(",",""); print $4+$6+$8+$10}' "$f"); errs=${errs:-0}
    non=$(awk '/Non-2xx/ {print $NF}' "$f"); non=${non:-0}
    if [[ -n "$req" && "$req" -gt 0 && "$errs" == 0 && "$non" == 0 ]]; then ok "$what：$n 压测 $req 个请求（长连接 keep-alive），0 失败"
    else bad "$what：$n 压测 请求 ${req:-0}、套接字错误 $errs、非 2xx $non（$(grep -E 'Socket errors|Non-2xx' "$f" | tr '\n' ' ')）"; fi
    note "$what：$n 压测 ${req:-0} 请求，错误 $errs，非 2xx $non"
  done
  local pok pbad; read -r pok pbad <<<"$(probe_result "$ver")"
  (( pbad == 0 && pok > 0 )) && ok "$what：全部节点轮询 $pok 次，0 失败" || bad "$what：全部节点轮询 成功 $pok / 失败 $pbad（$(head -3 "$W/probe-$ver.err" 2>/dev/null | tr '\n' ' ')）"
  local wrong=0
  for i in $(seq 1 "$N"); do [[ $(docker exec "etos-n$i" readlink /opt/syapp/sbin/syapp) == */versions/$3/sbin/syapp ]] || wrong=$((wrong + 1)); done
  (( wrong == 0 )) && ok "$what：$N 个节点都运行 $3" || bad "$what：$wrong 个节点没有运行 $3"
  # 旧 master 在长连接结束后退出；等它们都退出再进行下一次升级
  for _ in $(seq 1 60); do
    local left=0
    for i in $(seq 1 "$N"); do docker exec "etos-n$i" test -e /run/syapp/syapp.pid.oldbin && left=$((left + 1)); done
    (( left == 0 )) && break; sleep 2
  done
}
rollout "热升级到 $UP" "$UP" "$UP"
rollout "回退到 $VER" "$VER" "$VER"
[[ $(curl -s --noproxy '*' --max-time 3 -H "$HOSTH" "http://$(ip_of 1)/") == *"scale v3"* ]] && ok "升级与回退后配置不变" || bad "升级与回退后配置异常"
docker exec etos-n1 ps -eo comm | grep -qi 'ngi''nx' && bad "节点进程名出现上游名" || ok "节点进程名为 syapp"

echo
if (( fail > 0 )); then echo "规模验收：$fail 项失败"; $DC logs --tail 30 console | sed 's/^/console | /'; docker logs --tail 20 etos-n1 2>&1 | sed 's/^/edge-01 | /'; exit 1; fi
echo "规模验收（$N 节点）：全部通过"
