#!/usr/bin/env bash
# 控制台冒烟测试：启动真实的 syapp-console（-tls-auto），对指定数据库走一遍主要 API。
# 用法：scripts/ci/console-smoke.sh <syapp-console> <syapp-license-server> [数据库 DSN，默认 SQLite]
# 控制台须是注入了 CI 临时公钥的测试构建；LIC_KEY / LIC_KID 指向对应私钥（与 demo-e2e 相同）。
set -uo pipefail
CONSOLE=$(realpath "$1"); LIC=$(realpath "$2"); DB=${3:-}
: "${LIC_KEY:?}" "${LIC_KID:?}"
export NO_PROXY='*' no_proxy='*'
W=$(mktemp -d); JAR=$W/jar; PORT=18643
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=console-smoke::$1"; fail=$((fail+1)); }
j()   { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
expect() { if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（期望包含 '$3'，实际 '${2:0:240}'）"; fi; }

args=(-listen 127.0.0.1:$PORT -grpc-listen 127.0.0.1:19643 -grpc-hosts 127.0.0.1 -data "$W/data" -tls-auto)
[[ -n "$DB" ]] && args+=(-db "$DB")
"$CONSOLE" "${args[@]}" > "$W/console.log" 2>&1 &
CPID=$!
trap 'kill $CPID 2>/dev/null' EXIT
B=https://localhost:$PORT/api/v1
for _ in $(seq 1 40); do [[ -f "$W/data/pki/web-ca.crt" ]] && curl -s --cacert "$W/data/pki/web-ca.crt" "$B/setup/status" >/dev/null && break; sleep 0.5; done
C=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -c "$JAR" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')

echo "== HTTPS（-tls-auto）"
expect "HTTPS 证书由内置 Web CA 签发且受信任" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -o /dev/null -w '%{http_code}' "$B/setup/status")" "200"
expect "可下载 Web CA 证书" "$(curl -s --cacert "$W/data/pki/web-ca.crt" "https://localhost:$PORT/web-ca.crt")" "BEGIN CERTIFICATE"
key=$(openssl s_client -connect 127.0.0.1:$PORT -servername localhost </dev/null 2>/dev/null | openssl x509 -noout -text 2>/dev/null | grep -m1 "Public Key Algorithm")
expect "证书为 ECDSA（浏览器可用）" "$key" "id-ecPublicKey"

echo "== 初始化与授权"
"${C[@]}" -X POST "$B/setup" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
"${C[@]}" -X POST "$B/auth/login" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
expect "登录后会话 Cookie 带 Secure" "$(awk '$6=="etos_session"{print $4}' "$JAR")" "TRUE"
INST=$(sed -E 's/.*"installation_id":"([^"]+)".*/\1/' "$W/data/installation.json")
"$LIC" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only -customer smoke -days 365 > "$W/l.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/l.lic" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "授权有效" "$("${C[@]}" "$B/license" | j "d['state']")" "VALID"

echo "== 上游、证书、站点"
"${C[@]}" -X POST "$B/upstreams" -d '{"group_id":1,"spec":{"name":"api","servers":[{"addr":"10.0.0.11:8080"}]}}' >/dev/null
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 -subj /CN=shop.example.com \
  -addext subjectAltName=DNS:shop.example.com -keyout "$W/c.key" -out "$W/c.crt" 2>/dev/null
cid=$(python3 -c "import json,sys;print(json.dumps({'name':'shop','cert_pem':open(sys.argv[1]).read(),'key_pem':open(sys.argv[2]).read()}))" "$W/c.crt" "$W/c.key" \
  | "${C[@]}" -X POST "$B/certificates" -d @- | j "d['id']")
r=$("${C[@]}" -X POST "$B/sites" -d "{\"group_id\":1,\"spec\":{\"name\":\"shop\",\"domains\":[\"shop.example.com\"],\"https\":{\"cert_id\":$cid},\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"static\",\"static\":{\"index_html\":\"hi\"}},{\"path\":\"/api/\",\"match\":\"prefix\",\"mode\":\"proxy\",\"proxy\":{\"upstream\":\"api\"}}]}}")
expect "创建 HTTPS 站点" "$r" '"cert_id"'
expect "预览包含证书与站点配置" "$("${C[@]}" "$B/node-groups/1/preview")" "conf.d/site-shop.conf"
d=$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{"note":"smoke"}')
id=$(echo "$d" | j "d['id']")
st=""
for _ in $(seq 1 20); do st=$("${C[@]}" "$B/deployments/$id" | j "d['status']"); [[ "$st" != "APPLYING" ]] && break; sleep 0.5; done
expect "发布完成（节点组暂无节点）" "$st" "APPLIED"
expect "生成版本 r000001" "$("${C[@]}" "$B/revisions?group_id=1")" "r000001"
expect "无变更时拒绝重复发布" "$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{}')" "NO_CHANGES"

echo "== 审计"
expect "审计哈希链完好" "$("${C[@]}" "$B/audit/verify")" '"intact":true'
if grep -q "PRIVATE KEY" "$W/console.log"; then bad "日志中出现私钥"; else ok "日志中没有私钥"; fi

echo "== 备份与恢复（恢复到新的 SQLite 数据目录后重新启动）"
kill $CPID 2>/dev/null; wait $CPID 2>/dev/null
printf 'smoke backup passphrase\n' > "$W/pass"; chmod 600 "$W/pass"
bargs=(-data "$W/data" -passphrase-file "$W/pass"); [[ -n "$DB" ]] && bargs+=(-db "$DB")
"$CONSOLE" backup "${bargs[@]}" -out "$W/console.etosbk" > "$W/backup.log" 2>&1 \
  && ok "备份完成" || bad "备份失败：$(cat "$W/backup.log")"
if grep -aq "PRIVATE KEY" "$W/console.etosbk"; then bad "备份未加密"; else ok "备份已加密"; fi
"$CONSOLE" restore -data "$W/data2" -in "$W/console.etosbk" -passphrase-file "$W/pass" > "$W/restore.log" 2>&1 \
  && ok "恢复完成" || bad "恢复失败：$(cat "$W/restore.log")"
cmp -s "$W/data/pki/ca.key" "$W/data2/pki/ca.key" && ok "节点 CA 私钥随备份恢复" || bad "节点 CA 私钥未恢复"
"$CONSOLE" -listen 127.0.0.1:18644 -grpc-listen 127.0.0.1:19644 -grpc-hosts 127.0.0.1 -data "$W/data2" -tls-auto > "$W/console2.log" 2>&1 &
CPID=$!
B2=https://localhost:18644/api/v1; JAR2=$W/jar2
for _ in $(seq 1 40); do [[ -f "$W/data2/pki/web-ca.crt" ]] && curl -s --cacert "$W/data2/pki/web-ca.crt" "$B2/setup/status" >/dev/null && break; sleep 0.5; done
C2=(curl -s --cacert "$W/data2/pki/web-ca.crt" -b "$JAR2" -c "$JAR2" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
"${C2[@]}" -X POST "$B2/auth/login" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
expect "恢复后可用原账号登录，版本还在" "$("${C2[@]}" "$B2/revisions?group_id=1")" "r000001"
expect "恢复后授权仍有效（安装 ID 一致）" "$("${C2[@]}" "$B2/license" | j "d['state']")" "VALID"
expect "恢复后审计哈希链完好" "$("${C2[@]}" "$B2/audit/verify")" '"intact":true'
expect "恢复操作写入审计" "$("${C2[@]}" "$B2/audit?action=system.restore")" "system.restore"

if [[ $fail -ne 0 ]]; then tail -20 "$W/console.log" | sed 's/^/::warning title=console log::/'; fi
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "CONSOLE SMOKE OK" || exit 1
