#!/usr/bin/env bash
# 规格书 12.3 P6 验收：“从 v0.1.0 升级数据库无数据丢失”。
# 用真实的 v0.1.0 控制台建库并写入数据，停掉后用当前控制台打开同一个数据库与数据目录，逐项核对。
# 用法：scripts/ci/upgrade-from-v010.sh <v0.1.0 syedge-console> <当前 syedge-console> <syedge-license-server> [数据库 DSN，默认 SQLite]
# 两个控制台都须是注入了同一个 CI 临时公钥的测试构建；LIC_KEY / LIC_KID 指向对应私钥。
# OLD_LIC：v0.1.0 的 syedge-license-server（签发 v0.1.0 能识别的授权；升级后当前控制台须照常认可它）。
set -uo pipefail
OLD=$(realpath "$1"); NEW=$(realpath "$2"); LIC=$(realpath "$3"); DB=${4:-}
OLD_LIC=$(realpath "${OLD_LIC:-$LIC}")
KIND=SQLite; [[ -n "$DB" ]] && KIND=PostgreSQL
: "${LIC_KEY:?}" "${LIC_KID:?}"
export NO_PROXY='*' no_proxy='*'
W=$(mktemp -d); JAR=$W/jar
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=upgrade-from-v0.1.0 ($KIND)::$1"; fail=$((fail+1)); }
j()   { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
expect() { if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（期望包含 '$3'，实际 '${2:0:300}'）"; fi; }
same()   { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1（升级前 '${2:0:300}'，升级后 '${3:0:300}'）"; fi; }
PW='Upgrade@2026!v010'

args=(-grpc-hosts 127.0.0.1 -data "$W/data")
[[ -n "$DB" ]] && args+=(-db "$DB")
CPID=
stop() { [[ -n "$CPID" ]] && kill "$CPID" 2>/dev/null && wait "$CPID" 2>/dev/null; CPID=; }
trap stop EXIT

echo "== v0.1.0 控制台：建库并写入数据"
"$OLD" -listen 127.0.0.1:18650 -grpc-listen 127.0.0.1:19650 "${args[@]}" > "$W/old.log" 2>&1 &
CPID=$!
B=http://127.0.0.1:18650/api/v1
for _ in $(seq 1 40); do curl -s "$B/setup/status" >/dev/null && break; sleep 0.5; done
C=(curl -s -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
"${C[@]}" -X POST "$B/setup" -d "{\"username\":\"admin\",\"password\":\"$PW\"}" >/dev/null
"${C[@]}" -X POST "$B/auth/login" -d "{\"username\":\"admin\",\"password\":\"$PW\"}" >/dev/null
INST=$(sed -E 's/.*"installation_id":"([^"]+)".*/\1/' "$W/data/installation.json")
"$OLD_LIC" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only -customer upgrade -edition professional -days 365 > "$W/l.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/l.lic" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "v0.1.0 授权有效" "$("${C[@]}" "$B/license" | j "d['state']")" "VALID"
"${C[@]}" -X POST "$B/node-groups" -d '{"name":"edge-bj","description":"北京机房","allowed_cidrs":"10.0.0.0/8","real_ip_from":"192.168.0.0/16"}' >/dev/null
"${C[@]}" -X POST "$B/users" -d "{\"username\":\"ops1\",\"password\":\"$PW-ops\",\"role\":\"operator\"}" >/dev/null
"${C[@]}" -X POST "$B/users" -d "{\"username\":\"viewer1\",\"password\":\"$PW-view\",\"role\":\"viewer\"}" >/dev/null
for g in 1 2; do
  "${C[@]}" -X POST "$B/upstreams" -d "{\"group_id\":$g,\"spec\":{\"name\":\"api\",\"servers\":[{\"addr\":\"10.0.0.1$g:8080\",\"weight\":3},{\"addr\":\"10.0.0.2$g:8080\"}]}}" >/dev/null
  "${C[@]}" -X POST "$B/sites" -d "{\"group_id\":$g,\"spec\":{\"name\":\"shop\",\"domains\":[\"shop$g.example.com\",\"www.shop$g.example.com\"],\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"static\",\"static\":{\"index_html\":\"商城 v1\"}},{\"path\":\"/api/\",\"match\":\"prefix\",\"mode\":\"proxy\",\"proxy\":{\"upstream\":\"api\"}}]}}" >/dev/null
done
"${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"blog","domains":["blog.example.com"],"routes":[{"path":"/","match":"prefix","mode":"deny"}]}}' >/dev/null
publish() { # <group> -> 部署状态
  local id st=""; id=$("${C[@]}" -X POST "$B/node-groups/$1/publish" -d '{"note":"v0.1.0 数据"}' | j "d.get('id','')")
  for _ in $(seq 1 20); do st=$("${C[@]}" "$B/deployments/$id" | j "d['status']"); [[ "$st" != "APPLYING" ]] && break; sleep 0.5; done
  echo "$st"
}
# v0.1.0 在 PostgreSQL 上发布会失败（临时版本号超出 revisions.id 的 varchar(16)，P6 已修复），
# 所以 PostgreSQL 上只迁移不含版本的数据；SQLite 上覆盖版本、部署与回滚。
OLDPUB=1; [[ -n "$DB" ]] && OLDPUB=0
(( OLDPUB )) && expect "v0.1.0 发布 r000001" "$(publish 1)" "APPLIED"
sid=$("${C[@]}" "$B/sites?group_id=1" | j "[s['id'] for s in d if s['spec']['name']=='shop'][0]")
spec=$("${C[@]}" "$B/sites/$sid" | python3 -c "import json,sys;d=json.load(sys.stdin);s=d['spec'];s['routes'].insert(1,{'path':'/v2/','match':'prefix','mode':'static','static':{'index_html':'v2'}});print(json.dumps({'spec':s,'version':d['version']}))")
expect "v0.1.0 修改站点" "$("${C[@]}" -X PUT "$B/sites/$sid" -d "$spec")" '"version":2'
if (( OLDPUB )); then
  expect "v0.1.0 发布 r000002" "$(publish 1)" "APPLIED"
  expect "v0.1.0 节点组 2 发布" "$(publish 2)" "APPLIED"
fi

# 升级前快照：规范化后的 JSON（只保留升级前后都应不变的字段）
snap() {
  echo "groups=$("${C[@]}" "$B/node-groups" | j "sorted((g['name'], g.get('description',''), g.get('current_revision_id','')) for g in d)")"
  echo "users=$("${C[@]}" "$B/users" | j "sorted((u['username'], u['role']) for u in d)")"
  echo "sites=$("${C[@]}" "$B/sites" | j "sorted((s['group_id'], json.dumps(s['spec'], sort_keys=True, ensure_ascii=False)) for s in d)")"
  echo "upstreams=$("${C[@]}" "$B/upstreams" | j "sorted((u['group_id'], json.dumps(u['spec'], sort_keys=True)) for u in d)")"
  for g in 1 2; do echo "revisions$g=$("${C[@]}" "$B/revisions?group_id=$g" | j "sorted((r['id'], r.get('sha256',''), r.get('note','')) for r in (d if isinstance(d, list) else d.get('items', [])))")"; done
  echo "deployments=$("${C[@]}" "$B/deployments" | j "sorted((x['id'], x['revision_id'], x['status']) for x in (d if isinstance(d, list) else d.get('items', [])))")"
}
before=$(snap)
audit_before=$("${C[@]}" "$B/audit?limit=1000" | j "sorted((a['id'], a['action'], a.get('hash','')) for a in (d if isinstance(d, list) else d.get('items', [])))")
expect "v0.1.0 审计链完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'
stop

echo "== 当前控制台：打开同一个数据库（自动迁移）"
rm -f "$JAR"
"$NEW" -listen 127.0.0.1:18651 -grpc-listen 127.0.0.1:19651 -tls-auto "${args[@]}" > "$W/new.log" 2>&1 &
CPID=$!
B=https://localhost:18651/api/v1
for _ in $(seq 1 60); do [[ -f "$W/data/pki/web-ca.crt" ]] && curl -s --cacert "$W/data/pki/web-ca.crt" "$B/setup/status" >/dev/null && break; sleep 0.5; done
C=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
expect "已初始化（不再要求 setup）" "$("${C[@]}" "$B/setup/status")" '"initialized":true'
"${C[@]}" -X POST "$B/auth/login" -d "{\"username\":\"admin\",\"password\":\"$PW\"}" >/dev/null
expect "管理员用原密码登录" "$("${C[@]}" "$B/auth/me")" '"username":"admin"'
same "安装 ID 不变" "$INST" "$(sed -E 's/.*"installation_id":"([^"]+)".*/\1/' "$W/data/installation.json")"
expect "授权仍然有效" "$("${C[@]}" "$B/license" | j "d['state']")" "VALID"
after=$(snap)
while IFS= read -r line; do
  k=${line%%=*}
  same "数据一致：$k" "$line" "$(grep -m1 "^$k=" <<<"$after")"
done <<<"$before"
audit_after=$("${C[@]}" "$B/audit?limit=1000" | j "sorted((a['id'], a['action'], a.get('hash','')) for a in (d if isinstance(d, list) else d.get('items', [])) if a['id'] <= $(python3 -c "import sys,ast;print(max([x[0] for x in ast.literal_eval(sys.argv[1])] or [0]))" "$audit_before"))")
same "历史审计记录逐条保留（含哈希）" "$audit_before" "$audit_after"
expect "升级后审计链完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'

echo "== 升级后旧数据可继续使用"
"${C[@]}" -X POST "$B/auth/logout" >/dev/null; rm -f "$JAR"
"${C[@]}" -X POST "$B/auth/login" -d "{\"username\":\"ops1\",\"password\":\"$PW-ops\"}" >/dev/null
expect "旧的运维账号可登录" "$("${C[@]}" "$B/auth/me")" '"role":"operator"'
"${C[@]}" -X POST "$B/auth/logout" >/dev/null; rm -f "$JAR"
"${C[@]}" -X POST "$B/auth/login" -d "{\"username\":\"admin\",\"password\":\"$PW\"}" >/dev/null
if (( OLDPUB )); then
  r=$("${C[@]}" -X POST "$B/node-groups/1/rollback" -d '{"revision_id":"r000001"}')
  id=$(echo "$r" | j "d.get('id','')" 2>/dev/null)
  st=""; for _ in $(seq 1 20); do st=$("${C[@]}" "$B/deployments/$id" | j "d['status']" 2>/dev/null); [[ "$st" != "APPLYING" ]] && break; sleep 0.5; done
  expect "回滚到 v0.1.0 时期的版本 r000001" "$st" "APPLIED"
fi
"${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"news","domains":["news.example.com"],"routes":[{"path":"/","match":"prefix","mode":"deny"}]}}' >/dev/null
d=$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{"note":"升级后"}')
id=$(echo "$d" | j "d.get('id','')" 2>/dev/null)
st=""; for _ in $(seq 1 20); do st=$("${C[@]}" "$B/deployments/$id" | j "d['status']" 2>/dev/null); [[ "$st" != "APPLYING" ]] && break; sleep 0.5; done
expect "升级后新建站点并发布" "$st" "APPLIED"
expect "升级后审计链仍完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'
stop
if grep -qiE 'panic|migrate:|fatal' "$W/new.log"; then bad "控制台日志有错误：$(grep -iE 'panic|migrate:|fatal' "$W/new.log" | head -3)"; fi

echo
if (( fail > 0 )); then echo "从 v0.1.0 升级：$fail 项失败（日志 $W）"; exit 1; fi
echo "从 v0.1.0 升级：全部通过（$KIND）"
rm -rf "$W"
