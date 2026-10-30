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
"$LIC" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only -customer smoke -edition professional -days 365 > "$W/l.lic"
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

echo "== 发布审批（P7）"
expect "可开启发布审批" "$("${C[@]}" -X PUT "$B/release-approval" -d '{"enabled":true}')" '"enabled":true'
"${C[@]}" -X POST "$B/users" -d '{"username":"rel2","password":"Smoke@2026!rel","role":"releaser"}' >/dev/null
"${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"blog","domains":["blog.example.com"],"routes":[{"path":"/","match":"prefix","mode":"deny"}]}}' >/dev/null
expect "开启后不能直接发布" "$("${C[@]}" -X POST "$B/node-groups/1/publish" -d '{}')" "APPROVAL_REQUIRED"
rq=$("${C[@]}" -X POST "$B/node-groups/1/release-requests" -d '{"note":"blog"}')
expect "提交发布申请" "$rq" '"status":"pending"'
rid=$(echo "$rq" | j "d['id']"); rrev=$(echo "$rq" | j "d['revision_id']")
expect "申请详情含差异" "$("${C[@]}" "$B/release-requests/$rid")" "conf.d/site-blog.conf"
expect "提交人不能自己批准" "$("${C[@]}" -X POST "$B/release-requests/$rid/approve" -d '{}')" "SELF_APPROVAL"
JAR2=$W/jar2; C2=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR2" -c "$JAR2" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
expect "新用户首次登录必须改口令" "$("${C2[@]}" -X POST "$B/auth/login" -d '{"username":"rel2","password":"Smoke@2026!rel"}')" '"must_change_password":true'
expect "改口令前不能调用其他接口" "$("${C2[@]}" "$B/sites")" "PASSWORD_CHANGE_REQUIRED"
expect "弱口令被拒绝" "$("${C2[@]}" -X PUT "$B/auth/password" -d '{"old_password":"Smoke@2026!rel","new_password":"weakpassword"}')" "WEAK_PASSWORD"
expect "修改口令" "$("${C2[@]}" -X PUT "$B/auth/password" -d '{"old_password":"Smoke@2026!rel","new_password":"Smoke@2026!rel-new"}')" '"ok":true'
expect "不能改回历史口令" "$("${C2[@]}" -X PUT "$B/auth/password" -d '{"old_password":"Smoke@2026!rel-new","new_password":"Smoke@2026!rel"}')" "PASSWORD_REUSED"
d=$("${C2[@]}" -X POST "$B/release-requests/$rid/approve" -d '{"comment":"ok"}')
expect "另一位发布员批准后下发冻结的版本" "$d" "\"revision_id\":\"$rrev\""
id=$(echo "$d" | j "d['id']")
for _ in $(seq 1 20); do st=$("${C[@]}" "$B/deployments/$id" | j "d['status']"); [[ "$st" != "APPLYING" ]] && break; sleep 0.5; done
expect "审批后的发布完成" "$st" "APPLIED"
expect "申请状态为已批准" "$("${C[@]}" "$B/release-requests/$rid")" '"status":"approved"'
expect "审计记录审批" "$("${C[@]}" "$B/audit?limit=20")" "release.approve"
expect "关闭发布审批" "$("${C[@]}" -X PUT "$B/release-approval" -d '{"enabled":false}')" '"enabled":false'

echo "== 自定义片段（P7）"
"${C[@]}" -X POST "$B/users" -d '{"username":"op2","password":"Smoke@2026!opx","role":"operator"}' >/dev/null
sid=$("${C[@]}" "$B/sites?group_id=1" | j "[x['id'] for x in d if x['name']=='shop'][0]")
expect "禁用指令被拦下并给出行号" "$("${C[@]}" -X POST "$B/snippets" -d "{\"group_id\":1,\"site_id\":$sid,\"scope\":\"server\",\"content\":\"gzip on;\\ninclude /etc/passwd;\"}")" '"line":2'
sn=$("${C[@]}" -X POST "$B/snippets" -d "{\"group_id\":1,\"site_id\":$sid,\"scope\":\"server\",\"content\":\"client_max_body_size 50m;\"}")
expect "提交片段" "$sn" '"pending_by":"admin"'
snid=$(echo "$sn" | j "d['id']")
pv=$("${C[@]}" "$B/node-groups/1/preview")
if [[ "$pv" == *client_max_body_size* ]]; then bad "待审批片段不应参与渲染"; else ok "待审批片段不参与渲染"; fi
expect "提交人不能批准自己的片段" "$("${C[@]}" -X POST "$B/snippets/$snid/approve" -d '{}')" "SELF_APPROVAL"
JAR3=$W/jar3; C3=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR3" -c "$JAR3" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
"${C3[@]}" -X POST "$B/auth/login" -d '{"username":"op2","password":"Smoke@2026!opx"}' >/dev/null
"${C3[@]}" -X PUT "$B/auth/password" -d '{"old_password":"Smoke@2026!opx","new_password":"Smoke@2026!opx-new"}' >/dev/null
expect "另一位运维批准片段" "$("${C3[@]}" -X POST "$B/snippets/$snid/approve" -d '{}')" '"ok":true'
expect "批准后的片段进入待发布变更" "$("${C[@]}" "$B/node-groups/1/preview")" "client_max_body_size  50m;"

echo "== 配置导入（P7）"
cat > "$W/dump.txt" <<'DUMP'
# configuration file /etc/web/web.conf:
worker_processes auto;
http {
    upstream legacy_api { server 10.0.0.21:8080; }
    include conf.d/*.conf;
}
# configuration file /etc/web/conf.d/imp.conf:
server {
    listen 80;
    server_name imp.example.com;
    client_max_body_size 8m;
    location /api/ { proxy_pass http://legacy_api; proxy_read_timeout 90s; }
    location ~ \.php$ { fastcgi_pass 127.0.0.1:9000; }
}
DUMP
imp=$(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -H 'X-Requested-With: etos' -F group_id=1 -F "file=@$W/dump.txt" "$B/imports")
expect "生成导入报告" "$imp" '"partial":1'
expect "无法转换的指令带文件名与行号" "$imp" '"file":"/etc/web/conf.d/imp.conf","line":6,"directive":"fastcgi_pass"'
iid=$(echo "$imp" | j "d['import']['id']")
ap=$("${C[@]}" -X POST "$B/imports/$iid/apply" -d '{}')
expect "应用导入：生成上游" "$ap" '"legacy-api"'
expect "需人工处理的站点导入后停用" "$ap" '"enabled":false'
expect "片段提交审批" "$("${C[@]}" "$B/snippets?group_id=1")" '"origin":"import"'
expect "不能重复应用" "$("${C[@]}" -X POST "$B/imports/$iid/apply" -d '{}')" "ALREADY_APPLIED"
expect "报告可下载" "$("${C[@]}" "$B/imports/$iid/report")" '"summary"'

echo "== 开放 API 令牌（P9）"
tk=$("${C[@]}" -X POST "$B/api-tokens" -d '{"name":"ci","role":"operator","expire_days":30}')
expect "创建令牌（明文只返回一次）" "$tk" '"token":"etos_'
TOK=$(echo "$tk" | j "d['token']"); tid=$(echo "$tk" | j "d['info']['id']")
T=(curl -s --cacert "$W/data/pki/web-ca.crt" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json')
expect "令牌可调用读接口" "$("${T[@]}" "$B/sites?group_id=1")" '"name":"shop"'
expect "令牌调用写接口不需要 CSRF 头" "$("${T[@]}" -X POST "$B/upstreams" -d '{"group_id":1,"spec":{"name":"tok-up","servers":[{"addr":"10.0.0.31:80"}]}}')" '"tok-up"'
expect "令牌角色上限生效（operator 不能管理用户）" "$("${T[@]}" "$B/users")" "FORBIDDEN"
expect "令牌不能创建令牌" "$("${T[@]}" -X POST "$B/api-tokens" -d '{"name":"x"}')" "FORBIDDEN"
expect "OpenAPI 文档列出接口" "$("${T[@]}" "$B/openapi.json")" '"/sites/{id}"'
if grep -q "$TOK" "$W/console.log" "$W/data/"*.db 2>/dev/null; then bad "令牌明文出现在日志或数据库中"; else ok "令牌明文不落库、不进日志"; fi
"${C[@]}" -X DELETE "$B/api-tokens/$tid" >/dev/null
expect "撤销后令牌失效" "$("${T[@]}" "$B/sites")" "UNAUTHORIZED"

echo "== 告警（P9）"
expect "创建钉钉通道（密钥加密保存）" "$("${C[@]}" -X POST "$B/alert-channels" -d '{"name":"dt","type":"dingtalk","url":"https://oapi.example.com/robot/send?access_token=x","secret":"SECsmoke","enabled":true}')" '"type":"dingtalk"'
expect "保存告警规则" "$("${C[@]}" -X PUT "$B/alert-rules/cert_expiry" -d '{"enabled":true,"threshold":30,"channel_ids":[1]}')" '"ok":true'
al=$("${C[@]}" "$B/alerts")
expect "告警页数据" "$al" '"has_secret":true'
if [[ "$al" == *SECsmoke* ]]; then bad "接口返回了通道密钥"; else ok "接口不返回通道密钥"; fi

if [[ -n "${PKG_KEY:-}" ]]; then
  echo "== 数据面版本库（P8）"
  mkdir -p "$W/pkg/sbin" && printf '#!/bin/sh\nexit 0\n' > "$W/pkg/sbin/syapp" && chmod 755 "$W/pkg/sbin/syapp" && echo 9.9.1 > "$W/pkg/VERSION"
  tar -C "$W/pkg" -czf "$W/syapp-9.9.1-amd64.tar.gz" sbin/syapp VERSION
  "$LIC" sign-package -key "$PKG_KEY" -kid etos-ci-pkg-ephemeral -in "$W/syapp-9.9.1-amd64.tar.gz" -version 9.9.1 -arch amd64 >/dev/null
  P=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -H 'X-Requested-With: etos')
  expect "导入签名升级包" "$("${P[@]}" -F "package=@$W/syapp-9.9.1-amd64.tar.gz" -F "signature=@$W/syapp-9.9.1-amd64.tar.gz.sig" "$B/packages")" '"version":"9.9.1"'
  printf 'x' >> "$W/syapp-9.9.1-amd64.tar.gz"
  expect "篡改的升级包被拒绝" "$("${P[@]}" -F "package=@$W/syapp-9.9.1-amd64.tar.gz" -F "signature=@$W/syapp-9.9.1-amd64.tar.gz.sig" "$B/packages")" "PACKAGE_REJECTED"
  expect "版本库列表" "$("${C[@]}" "$B/packages")" '"kid":"etos-ci-pkg-ephemeral"'
  expect "节点组没有节点时拒绝升级" "$("${C[@]}" -X POST "$B/node-groups/1/upgrade" -d '{"version":"9.9.1"}')" "没有节点"
fi

echo "== 账号安全（等保 2.0，P9）"
expect "读取安全策略" "$("${C[@]}" "$B/security-policy")" '"lock_threshold":5'
"${C[@]}" -X POST "$B/users" -d '{"username":"lk1","password":"Smoke@2026!lkx","role":"viewer"}' >/dev/null
for i in 1 2 3 4 5; do curl -s --cacert "$W/data/pki/web-ca.crt" -H 'X-Requested-With: etos' -H 'Content-Type: application/json' -X POST "$B/auth/login" -d '{"username":"lk1","password":"wrong-Pass#1"}' >/dev/null; done
expect "连续失败后账号锁定" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -H 'X-Requested-With: etos' -H 'Content-Type: application/json' -X POST "$B/auth/login" -d '{"username":"lk1","password":"Smoke@2026!lkx"}')" "ACCOUNT_LOCKED"
lid=$("${C[@]}" "$B/users" | j "[u['id'] for u in d if u['username']=='lk1'][0]")
"${C[@]}" -X PUT "$B/users/$lid" -d '{"unlock":true}' >/dev/null
expect "管理员解锁后可登录" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -H 'X-Requested-With: etos' -H 'Content-Type: application/json' -X POST "$B/auth/login" -d '{"username":"lk1","password":"Smoke@2026!lkx"}')" '"username":"lk1"'
JAR4=$W/jar4; C4=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR4" -c "$JAR4" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
"${C4[@]}" -X POST "$B/auth/login" -d '{"username":"lk1","password":"Smoke@2026!lkx"}' >/dev/null
"${C4[@]}" -X PUT "$B/auth/password" -d '{"old_password":"Smoke@2026!lkx","new_password":"Smoke@2026!lkx-2"}' >/dev/null
expect "停用前会话可用" "$("${C4[@]}" "$B/auth/me")" '"username":"lk1"'
"${C[@]}" -X PUT "$B/users/$lid" -d '{"disabled":true}' >/dev/null
expect "停用立即生效（已有会话失效）" "$("${C4[@]}" "$B/auth/me")" "UNAUTHORIZED"
expect "不能删除自己" "$("${C[@]}" -X DELETE "$B/users/1")" "SELF"
expect "OIDC 设置要求 https" "$("${C[@]}" -X PUT "$B/sso/oidc" -d '{"enabled":true,"issuer":"http://sso.example.com","client_id":"c","redirect_url":"https://x/api/v1/auth/oidc/callback"}')" "BAD_OIDC"
expect "保存 OIDC 设置（密钥不回显）" "$("${C[@]}" -X PUT "$B/sso/oidc" -d '{"enabled":true,"issuer":"https://sso.invalid","client_id":"console","client_secret":"oidc-secret-x","redirect_url":"https://console.invalid/api/v1/auth/oidc/callback"}')" '"has_client_secret":true'
expect "登录页显示单点登录" "$("${C[@]}" "$B/setup/status")" '"oidc":true'
"${C[@]}" -X PUT "$B/sso/oidc" -d '{"enabled":false,"issuer":"https://sso.invalid","client_id":"console","redirect_url":"https://console.invalid/api/v1/auth/oidc/callback"}' >/dev/null
expect "LDAP 设置（需 ldaps 或 StartTLS）" "$("${C[@]}" -X PUT "$B/sso/ldap" -d '{"enabled":true,"url":"http://x","base_dn":"dc=x","user_filter":"(uid=%s)"}')" "BAD_LDAP"

echo "== 批量与灰度发布（P9）"
expect "批量发布逐组返回结果" "$("${C[@]}" -X POST "$B/releases/batch" -d '{"group_ids":[1],"note":"batch"}')" '"results":[{"group_id":1'
expect "专业版不含灰度发布" "$("${C[@]}" -X POST "$B/node-groups/1/canary" -d '{}')" "LICENSE_FEATURE_DENIED"

echo "== 静态资源发布（P9）"
python3 -c "import zipfile,sys; z=zipfile.ZipFile(sys.argv[1],'w'); z.writestr('dist/index.html','<h1>static v1</h1>'); z.writestr('dist/js/app.js','console.log(1)'); z.close()" "$W/web.zip"
sbid=$(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -H 'X-Requested-With: etos' -F group_id=1 -F name=web-v1 -F "file=@$W/web.zip" "$B/static-bundles" | j "d['id']")
expect "上传静态资源包" "$("${C[@]}" "$B/static-bundles?group_id=1")" '"files":2'
"${C[@]}" -X POST "$B/sites" -d "{\"group_id\":1,\"spec\":{\"name\":\"web\",\"domains\":[\"web.example.com\"],\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"static\",\"static\":{\"bundle_id\":$sbid}}]}}" >/dev/null
expect "资源包文件进入待发布版本" "$("${C[@]}" "$B/node-groups/1/preview")" "/js/app.js"
expect "被引用的资源包不能删除" "$("${C[@]}" -X DELETE "$B/static-bundles/$sbid")" "IN_USE"

echo "== 四层代理（P9）"
expect "保留端口被拒绝" "$("${C[@]}" -X POST "$B/streams" -d '{"group_id":1,"spec":{"name":"bad","listen":443,"servers":[{"addr":"10.0.0.41:3306"}]}}')" "INVALID_STREAM"
expect "回源网段之外被拒绝" "$("${C[@]}" -X POST "$B/streams" -d '{"group_id":1,"spec":{"name":"bad","listen":3306,"servers":[{"addr":"169.254.169.254:80"}]}}')" "INVALID_STREAM"
expect "创建四层代理" "$("${C[@]}" -X POST "$B/streams" -d '{"group_id":1,"spec":{"name":"mysql","listen":3306,"servers":[{"addr":"10.0.0.41:3306"}]}}')" '"listen":3306'
expect "四层代理进入待发布变更" "$("${C[@]}" "$B/node-groups/1/preview")" "stream.d/streams.conf"

echo "== 国密 TLCP 双证书（P8 国密变体）"
# 系统 OpenSSL 3 内置 SM2：生成真实的 SM2-with-SM3 签名证书与加密证书
for u in sign:digitalSignature enc:keyEncipherment,dataEncipherment; do
  n=${u%%:*}; ku=${u#*:}
  openssl genpkey -algorithm SM2 -out "$W/gm-$n.key" 2>/dev/null
  openssl req -new -x509 -key "$W/gm-$n.key" -sm3 -days 30 -subj /CN=gm.example.com -addext subjectAltName=DNS:gm.example.com \
    -addext "keyUsage=critical,$ku" -out "$W/gm-$n.crt" 2>/dev/null
done
gmreq() { python3 -c "import json,sys;r=lambda p:open(p).read();print(json.dumps({'name':sys.argv[1],'sign_cert_pem':r(sys.argv[2]),'sign_key_pem':r(sys.argv[3]),'enc_cert_pem':r(sys.argv[4]),'enc_key_pem':r(sys.argv[5])}))" "$@"; }
expect "专业版不能上传国密证书" "$(gmreq gm "$W/gm-sign.crt" "$W/gm-sign.key" "$W/gm-enc.crt" "$W/gm-enc.key" | "${C[@]}" -X POST "$B/certificates/gm" -d @-)" "LICENSE_FEATURE_DENIED"
"$LIC" issue -key "$LIC_KEY" -kid "$LIC_KID" -installation-id "$INST" -binding installation_only -customer smoke -edition enterprise -days 365 > "$W/le.lic"
python3 -c "import json,sys;print(json.dumps({'raw':open(sys.argv[1]).read()}))" "$W/le.lic" | "${C[@]}" -X POST "$B/license/import" -d @- >/dev/null
expect "签名证书与加密证书放反被拒绝" "$(gmreq gm "$W/gm-enc.crt" "$W/gm-enc.key" "$W/gm-sign.crt" "$W/gm-sign.key" | "${C[@]}" -X POST "$B/certificates/gm" -d @-)" "放反"
r=$(gmreq gm "$W/gm-sign.crt" "$W/gm-sign.key" "$W/gm-enc.crt" "$W/gm-enc.key" | "${C[@]}" -X POST "$B/certificates/gm" -d @-)
expect "企业版上传国密双证书" "$r" '"kind":"sm2"'
if [[ "$r" == *PRIVATE* ]]; then bad "国密证书响应中出现私钥"; else ok "国密证书响应中没有私钥"; fi
gid=$(echo "$r" | j "d['id']")
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 -subj /CN=gm.example.com \
  -addext subjectAltName=DNS:gm.example.com -keyout "$W/gs.key" -out "$W/gs.crt" 2>/dev/null
sid=$(python3 -c "import json,sys;print(json.dumps({'name':'gm-std','cert_pem':open(sys.argv[1]).read(),'key_pem':open(sys.argv[2]).read()}))" "$W/gs.crt" "$W/gs.key" \
  | "${C[@]}" -X POST "$B/certificates" -d @- | j "d['id']")
r=$("${C[@]}" -X POST "$B/sites" -d "{\"group_id\":1,\"spec\":{\"name\":\"gm\",\"domains\":[\"gm.example.com\"],\"https\":{\"cert_id\":$sid,\"gm_cert_id\":$gid},\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"deny\"}]}}")
expect "创建国密站点（标准证书 + 国密双证书）" "$r" "\"gm_cert_id\":$gid"
p=$("${C[@]}" "$B/node-groups/1/preview")
expect "预览包含国密双证书文件" "$p" "certs/site-gm.sm2-enc.key"
expect "预览包含国密指令" "$p" "Enable_ntls"
if [[ "$p" == *"BEGIN PRIVATE KEY"* ]]; then bad "预览中出现私钥"; else ok "预览中没有私钥"; fi
expect "有国密站点的节点组不能升级到标准版" "$("${C[@]}" -X POST "$B/node-groups/1/upgrade" -d '{"version":"9.9.1","variant":"standard"}')" "国密"
expect "国密证书被引用时不能删除" "$("${C[@]}" -X DELETE "$B/certificates/$gid")" "正被站点使用"

echo "== 规则积木与 Lua 规则（P10）"
expect "创建 IP 集" "$("${C[@]}" -X POST "$B/ip-sets" -d '{"group_id":1,"name":"office","label":"办公网","cidrs":["10.20.0.0/16","192.0.2.7"]}')" '"count":2'
expect "非法网段被拒绝" "$("${C[@]}" -X POST "$B/ip-sets" -d '{"group_id":1,"name":"bad","cidrs":["10.0.0.0/33"]}')" "INVALID_IP_SET"
printf 'apnic|CN|ipv4|1.0.1.0|256|20110414|allocated\napnic|CN|ipv4|1.0.2.0|768|20110414|allocated\napnic|JP|ipv4|1.0.16.0|4096|20110412|allocated\n' > "$W/delegated.txt"
expect "从 RIR 分配文件导入地域库" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JAR" -H 'X-Requested-With: etos' -F group_id=1 -F name=cn -F country=CN -F file=@"$W/delegated.txt" "$B/ip-sets/import")" '"source":"rir:CN"'
RULES='[{"name":"block-cn-admin","conditions":[{"kind":"ip_set","set":"cn"},{"kind":"path","op":"prefix","value":"/admin/"}],"action":{"type":"deny","status":403}},{"name":"bots","conditions":[{"kind":"user_agent","op":"contains","value":"BadBot\"; return 200"}],"action":{"type":"deny","status":444}},{"name":"api-limit","conditions":[{"kind":"path","op":"prefix","value":"/api/"}],"action":{"type":"rate_limit","rate":20,"per":"s","burst":40}},{"name":"api-jwt","conditions":[{"kind":"path","op":"prefix","value":"/v1/"}],"action":{"type":"jwt","jwt":{"alg":"HS256","secret":"smoke-secret-0123456789abcdef0123456789","source":"header"}}}]'
r=$("${C[@]}" -X POST "$B/sites" -d "{\"group_id\":1,\"spec\":{\"name\":\"rules\",\"domains\":[\"rules.example.com\"],\"routes\":[{\"path\":\"/\",\"match\":\"prefix\",\"mode\":\"deny\"}],\"rules\":$RULES}}")
expect "创建带规则的站点" "$r" '"block-cn-admin"'
expect "响应中只标明已设置密钥" "$r" '"has_secret":true'
if [[ "$r" == *smoke-secret* || "$r" == *secret_enc* ]]; then bad "站点响应中出现 JWT 密钥"; else ok "站点响应中没有 JWT 密钥（明文与密文）"; fi
p=$("${C[@]}" "$B/node-groups/1/preview")
expect "预览包含地域库" "$p" "conf.d/00-ipsets.conf"
expect "预览包含规则变量" "$p" "etos_rules_r0"
expect "预览包含限速" "$p" "limit_req_zone"
expect "预览包含 Lua 初始化" "$p" "init_by_lua_block"
expect "预览包含 JWT 密钥文件（内容不显示）" "$p" "lua/jwt-rules-api-jwt.key"
if [[ "$p" == *smoke-secret* ]]; then bad "预览中出现 JWT 密钥"; else ok "预览中没有 JWT 密钥"; fi
expect "引用中的 IP 集不能删除" "$("${C[@]}" -X DELETE "$B/ip-sets/$("${C[@]}" "$B/ip-sets?group_id=1" | j "[x['id'] for x in d if x['name']=='cn'][0]")")" "IN_USE"
expect "非法规则被拒绝" "$("${C[@]}" -X POST "$B/sites" -d '{"group_id":1,"spec":{"name":"bad-rule","domains":["bad.example.com"],"routes":[{"path":"/","match":"prefix","mode":"deny"}],"rules":[{"name":"x","conditions":[],"action":{"type":"redirect","status":302,"to":"https://a.example.com/$host"}}]}}')" "INVALID_SITE"

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

if [[ -n "$DB" ]]; then
  echo "== 主备高可用（P9，同一个 PostgreSQL）"
  kill $CPID 2>/dev/null; wait $CPID 2>/dev/null
  # 备控制台的数据目录只复制 CA 与数据密钥；安装 ID 从共用数据库继承
  mkdir -p "$W/data-b" && cp -r "$W/data/pki" "$W/data/secret.key" "$W/data-b/"
  "$CONSOLE" "${args[@]}" > "$W/console-a.log" 2>&1 & APID=$!
  for _ in $(seq 1 40); do curl -s --cacert "$W/data/pki/web-ca.crt" "https://localhost:$PORT/healthz" | grep -q '"role":"leader"' && break; sleep 0.5; done
  "$CONSOLE" -listen 127.0.0.1:18645 -grpc-listen 127.0.0.1:19645 -grpc-hosts 127.0.0.1 -data "$W/data-b" -tls-auto -db "$DB" > "$W/console-b.log" 2>&1 & BPID=$!
  trap 'kill $CPID $APID $BPID 2>/dev/null' EXIT
  HB=https://localhost:18645
  for _ in $(seq 1 40); do curl -s --cacert "$W/data/pki/web-ca.crt" "$HB/healthz" >/dev/null && break; sleep 0.5; done
  expect "先启动的控制台为主" "$(curl -s --cacert "$W/data/pki/web-ca.crt" "https://localhost:$PORT/healthz")" '"role":"leader"'
  expect "后启动的控制台为备，/healthz 返回 503" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -w ' %{http_code}' "$HB/healthz")" '"role":"standby"'
  expect "备控制台拒绝 API" "$(curl -s --cacert "$W/data/pki/web-ca.crt" "$HB/api/v1/setup/status")" '"STANDBY"'
  kill $APID; wait $APID 2>/dev/null
  for _ in $(seq 1 40); do curl -s --cacert "$W/data/pki/web-ca.crt" "$HB/healthz" | grep -q '"role":"leader"' && break; sleep 0.5; done
  expect "主控制台退出后备控制台接管" "$(curl -s --cacert "$W/data/pki/web-ca.crt" -w ' %{http_code}' "$HB/healthz")" '"role":"leader"'
  JB=$W/jarb; CB=(curl -s --cacert "$W/data/pki/web-ca.crt" -b "$JB" -c "$JB" -H 'X-Requested-With: etos' -H 'Content-Type: application/json')
  "${CB[@]}" -X POST "$HB/api/v1/auth/login" -d '{"username":"admin","password":"Smoke@2026!"}' >/dev/null
  expect "接管后授权仍有效（安装 ID 相同）" "$("${CB[@]}" "$HB/api/v1/license" | j "d['state']")" "VALID"
  expect "接管后数据一致" "$("${CB[@]}" "$HB/api/v1/streams?group_id=1")" '"mysql"'
  kill $BPID 2>/dev/null; wait $BPID 2>/dev/null
  if grep -q "PRIVATE KEY" "$W/console-a.log" "$W/console-b.log"; then bad "日志中出现私钥"; else ok "主备日志中没有私钥"; fi
fi

if [[ $fail -ne 0 ]]; then tail -20 "$W/console.log" | sed 's/^/::warning title=console log::/'; fi
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "CONSOLE SMOKE OK" || exit 1
