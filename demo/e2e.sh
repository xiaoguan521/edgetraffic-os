#!/usr/bin/env bash
# 演示环境端到端验证：按演示脚本把关键流程跑一遍并逐项断言。
# 在 demo/ 目录下、./demo.sh up 之后运行。CI（.github/workflows/demo-e2e.yml）与演示服务器均可使用。
#
# 授权：默认用 ./demo.sh license（需要 demo/keys/etos-demo-2026.key）；
#       CI 中设置 LIC_TOOL / LIC_KEY / LIC_KID 使用临时密钥签发。
set -uo pipefail
cd "$(dirname "$0")"
DC="docker compose"
P1=${NODE1_PORT:-8001}; P2=${NODE2_PORT:-8002}
B="http://127.0.0.1:${CONSOLE_PORT:-8443}/api/v1"
JAR=$(mktemp); trap 'rm -f $JAR' EXIT
C=(curl -s --noproxy '*' -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
H=(curl -s --noproxy '*' --max-time 5 -H 'Host: shop.demo.local')
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=demo-e2e::$1"; fail=$((fail+1)); }
j()   { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
expect() { if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（期望包含 '$3'，实际 '${2:0:200}'）"; fi; }

wait_deploy() { # $1 = deployment id
  local s=""
  for _ in $(seq 1 120); do
    s=$("${C[@]}" "$B/deployments/$1" | j "d['status']")
    [[ "$s" != "APPLYING" ]] && break
    sleep 1
  done
  "${C[@]}" "$B/deployments/$1"
}
publish() { # $1 = note -> prints deployment json
  local id
  id=$("${C[@]}" -X POST "$B/node-groups/1/publish" -d "{\"note\":\"$1\"}" | j "d.get('id','')")
  [[ -z "$id" ]] && { echo '{"status":"REFUSED","nodes":[]}'; return; }
  wait_deploy "$id"
}

echo "== 控制台就绪"
for _ in $(seq 1 90); do curl -sf --noproxy '*' "$B/setup/status" >/dev/null && break; sleep 2; done
st=$(curl -s --noproxy '*' "$B/setup/status")
expect "控制台 API 可访问" "$st" "initialized"
expect "前端页面可访问" "$(curl -s --noproxy '*' "http://127.0.0.1:${CONSOLE_PORT:-8443}/login")" "EdgeTraffic OS"

echo "== 初始化与授权"
"${C[@]}" -X POST "$B/setup" -d '{"username":"admin","password":"Demo@2026!"}' >/dev/null
"${C[@]}" -X POST "$B/auth/login" -d '{"username":"admin","password":"Demo@2026!"}' >/dev/null
expect "管理员登录" "$("${C[@]}" "$B/auth/me")" '"admin"'
INST=$($DC exec -T console sh -c 'cat /var/lib/syapp-console/installation.json' | sed -E 's/.*"installation_id":"([^"]+)".*/\1/')
issue() { # $@ = extra args; prints license text
  if [[ -n "${LIC_TOOL:-}" ]]; then
    "$LIC_TOOL" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only \
      -customer "演示客户" -edition professional -nodes 10 -sites 200 "$@"
  else
    OUT=e2e.lic ./demo.sh license >/dev/null && cat out/e2e.lic
  fi
}
issue -days 365 > /tmp/demo-valid.lic
python3 -c "import json;print(json.dumps({'raw':open('/tmp/demo-valid.lic').read()}))" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "导入授权" "$("${C[@]}" "$B/license" | j "d['state']")" "VALID"

echo "== 添加两台节点"
for n in node1 node2; do
  cmd=$("${C[@]}" -X POST "$B/node-groups/1/enroll" | j "d['command']")
  ./demo.sh enroll "$n" $cmd >/dev/null
done
online=0
for _ in $(seq 1 60); do
  online=$("${C[@]}" "$B/nodes" | j "sum(1 for n in d if n['status']=='online')")
  [[ "$online" == "2" ]] && break
  sleep 2
done
expect "两台节点上线" "$online" "2"
expect "节点进程名为 syapp" "$($DC exec -T node1 ps -eo args)" "syapp: master process"
if $DC exec -T node1 sh -c 'ps -eo args; syapp -v 2>&1; syapp -h 2>&1' | grep -qi 'ngi''nx'; then bad "节点进程与命令行中出现上游名"; else ok "节点进程与命令行中无上游名"; fi

echo "== 建上游与站点并发布"
"${C[@]}" -X POST "$B/upstreams" -d '{"group_id":1,"spec":{"name":"order-api","algorithm":"least_conn","servers":[{"addr":"172.30.0.31:9001"},{"addr":"172.30.0.32:9001"}]}}' >/dev/null
SITE='{"group_id":1,"spec":{"name":"shop","domains":["shop.demo.local"],"routes":[{"path":"/","match":"prefix","mode":"static","static":{"index_html":"<h1>演示商城 v1</h1>"}},{"path":"/api/","match":"prefix","mode":"proxy","proxy":{"upstream":"order-api"}},{"path":"/admin/","match":"prefix","mode":"deny"}]}}'
"${C[@]}" -X POST "$B/sites" -d "$SITE" >/dev/null
d=$(publish "上线演示商城")
expect "首次发布全部生效" "$(echo "$d" | j "d['status']+' '+','.join(n['status'] for n in d['nodes'])")" "APPLIED APPLIED,APPLIED"
expect "节点 1 返回 v1 页面" "$("${H[@]}" "http://127.0.0.1:$P1/")" "演示商城 v1"
expect "节点 2 返回 v1 页面" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城 v1"
expect "反向代理到后端" "$("${H[@]}" "http://127.0.0.1:$P1/api/orders")" '"service":"order-api"'
expect "拒绝访问路由" "$("${H[@]}" -o /dev/null -w '%{http_code}' "http://127.0.0.1:$P2/admin/")" "403"
expect "响应头 Server 为 syapp" "$("${H[@]}" -I "http://127.0.0.1:$P1/")" "Server: syapp"

echo "== 内容更新 v2"
ver=$("${C[@]}" "$B/sites/1" | j "d['version']")
"${C[@]}" -X PUT "$B/sites/1" -d "$(echo "$SITE" | sed "s/v1/v2/; s/\"group_id\":1/\"version\":$ver/")" >/dev/null
d=$(publish "首页改版 v2")
expect "v2 发布生效" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "节点返回 v2 页面" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城 v2"

echo "== 根路径改到宕机后端：探活失败自动回滚"
"${C[@]}" -X POST "$B/upstreams" -d '{"group_id":1,"spec":{"name":"new-api","servers":[{"addr":"172.30.0.99:9001"}]}}' >/dev/null
ver=$("${C[@]}" "$B/sites/1" | j "d['version']")
"${C[@]}" -X PUT "$B/sites/1" -d "{\"version\":$ver,\"spec\":{\"domains\":[\"shop.demo.local\"],\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"proxy\",\"proxy\":{\"upstream\":\"new-api\"}}]}}" >/dev/null
d=$(publish "切到新后端")
expect "节点自动回滚" "$(echo "$d" | j "','.join(n['status'] for n in d['nodes'])")" "ROLLED_BACK"
expect "回滚后仍返回 v2" "$("${H[@]}" "http://127.0.0.1:$P1/")" "演示商城 v2"

echo "== 一键回滚到 r000001"
id=$("${C[@]}" -X POST "$B/node-groups/1/rollback" -d '{"revision_id":"r000001"}' | j "d['id']")
d=$(wait_deploy "$id")
expect "回滚生效" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "节点返回 v1 页面" "$("${H[@]}" "http://127.0.0.1:$P1/")" "演示商城 v1"

echo "== 授权过期：只读但回滚与流量不受影响"
issue -expires "$(date -u -d '-20 days' +%F)" -grace 15 > /tmp/demo-expired.lic
python3 -c "import json;print(json.dumps({'raw':open('/tmp/demo-expired.lic').read()}))" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "授权状态为已过期" "$("${C[@]}" "$B/license" | j "d['state']")" "EXPIRED"
expect "过期后发布被拒绝" "$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{}')" "LICENSE_READ_ONLY"
id=$("${C[@]}" -X POST "$B/node-groups/1/rollback" -d '{"revision_id":"r000002"}' | j "d['id']")
d=$(wait_deploy "$id")
expect "过期后回滚仍可用" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "过期后流量正常" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城 v2"
python3 -c "import json;print(json.dumps({'raw':open('/tmp/demo-valid.lic').read()}))" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null

echo "== 审计"
expect "审计哈希链完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'

echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "DEMO E2E OK" || exit 1
