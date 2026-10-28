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
expect "发布后无待发布变更" "$("${C[@]}" "$B/node-groups/1/preview")" '"changed":false'
expect "无变更时文件列表为空数组" "$("${C[@]}" "$B/node-groups/1/preview")" '"files":[]'
expect "响应头 Server 为 syapp" "$("${H[@]}" -I "http://127.0.0.1:$P1/")" "Server: syapp"

echo "== 漂移检测：有人登录节点手工改了配置"
$DC exec -T node1 sh -c 'echo "# manual edit" >> /opt/syapp/conf/current/syapp.conf'
drift=""
for _ in $(seq 1 40); do
  drift=$("${C[@]}" "$B/nodes" | j "','.join(n['name']+'='+str(n['drifted']) for n in sorted(d, key=lambda n: n['name']))")
  [[ "$drift" == *"=True,"* ]] && break
  sleep 2
done
expect "控制台标记 node1 配置被改动" "$drift" "edge-sg-01=True"
expect "node2 未被误报" "$drift" "edge-sg-02=False"
n1=$("${C[@]}" "$B/nodes" | j "[n['id'] for n in d if n['name']=='edge-sg-01' and n['status']=='online'][0]")
id=$("${C[@]}" -X POST "$B/nodes/$n1/restore" | j "d.get('id','')")
d=$(wait_deploy "$id")
expect "一键恢复生效" "$(echo "$d" | j "d['kind']+' '+d['status']")" "restore APPLIED"
for _ in $(seq 1 25); do
  drift=$("${C[@]}" "$B/nodes" | j "','.join(n['name']+'='+str(n['drifted']) for n in sorted(d, key=lambda n: n['name']))")
  [[ "$drift" == *"edge-sg-01=False"* ]] && break
  sleep 2
done
expect "恢复后漂移标记消除" "$drift" "edge-sg-01=False"
if $DC exec -T node1 grep -q "manual edit" /opt/syapp/conf/current/syapp.conf; then bad "恢复后仍是手改的配置"; else ok "恢复后配置为控制台版本"; fi
expect "手工改动已备份" "$($DC exec -T node1 sh -c 'ls -a /opt/syapp/conf/releases')" ".drift-r000001-"
expect "恢复不改变组版本" "$("${C[@]}" "$B/node-groups" | j "d[0]['current_revision_id']")" "r000001"
expect "恢复后流量正常" "$("${H[@]}" "http://127.0.0.1:$P1/")" "演示商城 v1"
expect "无漂移时拒绝恢复" "$("${C[@]}" -X POST "$B/nodes/$n1/restore")" "NOT_DRIFTED"

echo "== 内容更新 v2"
ver=$("${C[@]}" "$B/sites/1" | j "d['version']")
"${C[@]}" -X PUT "$B/sites/1" -d "$(echo "$SITE" | sed "s/v1/v2/; s/\"group_id\":1/\"version\":$ver/")" >/dev/null
d=$(publish "首页改版 v2")
expect "v2 发布生效" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "节点返回 v2 页面" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城 v2"
for _ in $(seq 1 25); do
  drift=$("${C[@]}" "$B/nodes" | j "','.join(str(n['drifted']) for n in d)")
  [[ "$drift" != *True* ]] && break
  sleep 2
done
expect "重新发布后漂移标记消除" "$drift" "False,False"

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

echo "== 证书库：上传证书，站点启用 HTTPS"
CT=$(mktemp -d)
mkcert() { # $1 = 文件名前缀，$2 = 有效天数
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days "$2" -subj /CN=shop.demo.local \
    -addext subjectAltName=DNS:shop.demo.local -keyout "$CT/$1.key" -out "$CT/$1.crt" 2>/dev/null
}
certjson() { python3 -c "import json,sys;print(json.dumps({'name':sys.argv[1],'cert_pem':open(sys.argv[2]).read(),'key_pem':open(sys.argv[3]).read()}))" "$@"; }
enddate() { openssl x509 -enddate -noout -in "$1" | sed 's/notAfter=//'; }
served() { $DC exec -T node1 sh -c "curl -skv --max-time 5 --resolve shop.demo.local:443:127.0.0.1 https://shop.demo.local/ 2>&1" | sed -n 's/.*expire date: //p' | head -1; }
mkcert c30 30; mkcert c90 90; mkcert other 90
r=$(certjson shop "$CT/c30.crt" "$CT/other.key" | "${C[@]}" -X POST "$B/certificates" -d @-)
expect "证书与私钥不匹配被拒绝" "$r" "不匹配"
cid=$(certjson shop "$CT/c30.crt" "$CT/c30.key" | "${C[@]}" -X POST "$B/certificates" -d @- | j "d['id']")
expect "证书列表不含私钥" "$("${C[@]}" "$B/certificates")" '"status":"expiring"'
if "${C[@]}" "$B/certificates" | grep -q "PRIVATE KEY"; then bad "证书接口泄露私钥"; else ok "证书接口不返回私钥"; fi
expect "总览提示证书即将到期" "$("${C[@]}" "$B/dashboard")" '"certs_expiring":1'
ver=$("${C[@]}" "$B/sites/1" | j "d['version']")
r=$("${C[@]}" -X PUT "$B/sites/1" -d "{\"version\":$ver,\"spec\":{\"domains\":[\"shop.demo.local\",\"blog.demo.local\"],\"https\":{\"cert_id\":$cid},\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"static\",\"static\":{\"index_html\":\"<h1>演示商城 v3</h1>\"}}]}}")
expect "证书不覆盖的域名被拒绝" "$r" "blog.demo.local"
"${C[@]}" -X PUT "$B/sites/1" -d "{\"version\":$ver,\"spec\":{\"domains\":[\"shop.demo.local\"],\"https\":{\"cert_id\":$cid},\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"static\",\"static\":{\"index_html\":\"<h1>演示商城 v3</h1>\"}}]}}" >/dev/null
if "${C[@]}" "$B/sites/1" | grep -q "BEGIN"; then bad "站点配置中出现证书原文"; else ok "站点只保存证书编号"; fi
d=$(publish "启用 HTTPS")
expect "HTTPS 站点发布生效" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "节点使用证书库中的证书" "$(served)" "$(enddate "$CT/c30.crt")"

echo "== 授权过期：只读但回滚与流量不受影响"
issue -expires "$(date -u -d '-20 days' +%F)" -grace 15 -maintenance "$(date -u -d '+365 days' +%F)" > /tmp/demo-expired.lic
python3 -c "import json;print(json.dumps({'raw':open('/tmp/demo-expired.lic').read()}))" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "授权状态为已过期" "$("${C[@]}" "$B/license" | j "d['state']")" "EXPIRED"
expect "过期后发布被拒绝" "$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{}')" "LICENSE_READ_ONLY"
expect "过期后上传新证书被拒绝" "$(certjson extra "$CT/c90.crt" "$CT/c90.key" | "${C[@]}" -X POST "$B/certificates" -d @-)" "LICENSE_READ_ONLY"
r=$(certjson shop "$CT/c90.crt" "$CT/c90.key" | "${C[@]}" -X PUT "$B/certificates/$cid" -d @-)
expect "过期后证书续期仍可用" "$r" '"status":"started"'
id=$(echo "$r" | j "d['deployments'][0]['deployment_id']")
d=$(wait_deploy "$id")
expect "续期版本由节点放行并生效" "$(echo "$d" | j "d['kind']+' '+d['status']")" "cert_renew APPLIED"
expect "节点换上续期后的证书" "$(served)" "$(enddate "$CT/c90.crt")"
expect "续期后草稿与线上一致" "$("${C[@]}" "$B/node-groups/1/preview")" '"changed":false'
id=$("${C[@]}" -X POST "$B/node-groups/1/rollback" -d '{"revision_id":"r000002"}' | j "d['id']")
d=$(wait_deploy "$id")
expect "过期后回滚仍可用" "$(echo "$d" | j "d['status']")" "APPLIED"
expect "过期后流量正常" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城 v2"
python3 -c "import json;print(json.dumps({'raw':open('/tmp/demo-valid.lic').read()}))" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null

echo "== 吊销节点后重新添加"
n2=$("${C[@]}" "$B/nodes" | j "[n['id'] for n in d if n['name']=='edge-sg-02' and n['status']!='revoked'][0]")
"${C[@]}" -X POST "$B/nodes/$n2/revoke" >/dev/null
sleep 3
expect "吊销后节点状态" "$("${C[@]}" "$B/nodes" | j "[n['status'] for n in d if n['id']==$n2][0]")" "revoked"
cmd=$("${C[@]}" -X POST "$B/node-groups/1/enroll" | j "d['command']")
out=$(./demo.sh enroll node2 $cmd 2>&1)
expect "重新注册命令执行成功" "$out" "已注册"
st=""
for _ in $(seq 1 45); do
  st=$("${C[@]}" "$B/nodes" | j "','.join(n['status'] for n in d if n['name']=='edge-sg-02' and n['id']!=$n2)")
  [[ "$st" == "online" ]] && break
  sleep 2
done
expect "重新添加的节点上线（无需重启 agent）" "$st" "online"
expect "旧身份仍为吊销" "$("${C[@]}" "$B/nodes" | j "[n['status'] for n in d if n['id']==$n2][0]")" "revoked"
expect "重新添加后流量正常" "$("${H[@]}" "http://127.0.0.1:$P2/")" "演示商城"

echo "== 审计"
expect "审计哈希链完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'

echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "DEMO E2E OK" || exit 1
