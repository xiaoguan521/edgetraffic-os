#!/usr/bin/env bash
# P10 验收（规格书 12.5）：在一台干净的机器上，以生产路径验证规则积木与 syapp-lua 附加包。
#   用法：scripts/ci/lua-acceptance.sh <dist-lua 目录> <standard|gmssl>
#   需要：sudo、go、python3、openssl、curl、wrk；PKG_KEY / PKG_KID 为 CI 升级包签名私钥与编号，
#         WRAPPER 为注入了对应公钥（pkgsig.ciKey）的 syapp-wrapper，LIC 为 syapp-license-server。
# 覆盖：模块加载与真实 syapp -t；原生规则（IP / UA / 路径 / 跳转 / 限速 / 外部鉴权）；JWT HS256 / RS256；
#      Lua 沙箱；reload；带附加包的热升级与中止；标准 HTTPS 与国密 NTLS；附加包安装与卸载；品牌检查；
#      三条常用规则下 QPS 下降不超过 15%。
set -uo pipefail
DIST=$(realpath "$1"); BASE=$2
: "${PKG_KEY:?}" "${PKG_KID:?}" "${WRAPPER:?}" "${LIC:?}"
ARCH=$(uname -m); GOARCH=amd64; [[ $ARCH == aarch64 ]] && GOARCH=arm64
W=$(mktemp -d); chmod 755 "$W"
# 意外退出（例如命令不存在、变量未定义）时也留下注解：原始日志不一定能下载
trap 'rc=$?; [[ $rc -ne 0 && -z "${DONE:-}" ]] && echo "::error title=lua-acceptance aborted ($BASE/$(uname -m))::exit $rc at line $LINENO: $BASH_COMMAND"' EXIT
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; echo "::error title=lua-acceptance ($BASE/$ARCH)::$1"; fail=$((fail+1)); }
check() { if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1 — 期望包含 '$3'，实际 '${2:0:200}'"; fi; }
nocheck() { if [[ "$2" == *"$3"* ]]; then bad "$1 — 不应包含 '$3'"; else ok "$1"; fi; }
die() { bad "$1"; echo "---- $fail failed"; DONE=1; exit 1; }
# 每个功能请求用不同的客户端地址（X-Forwarded-For，验收配置信任本机），避免触发基线的单 IP 限速
# （在 $(...) 子 shell 中调用，计数器不会回传：地址取自 /dev/urandom）
req() {
  local r; r=$(od -An -N3 -tu1 /dev/urandom | awk '{print 200+$1%50"."$2"."($3%250)+1}')
  curl -s -o "$W/body" -w '%{http_code}' -H "X-Forwarded-For: 10.$r" "$@" 2>/dev/null
}
body() { cat "$W/body"; }

SY=/opt/syapp/sbin/syapp
# 压测时分开 CPU，避免前端、后端与压测工具互相争抢（runner 只有 4 个 CPU）：
# 前端 syapp（被测对象）0-1，后端 2，wrk 3。CPU 不足 4 个时不绑定。
if (( $(nproc) >= 4 )); then PIN_FRONT=(taskset -c 0,1); PIN_BACK=(taskset -c 2); PIN_WRK=(taskset -c 3); else PIN_FRONT=(); PIN_BACK=(); PIN_WRK=(); fi
basepkg=$(ls "$DIST"/base/syapp-*-"$ARCH".tar.gz | grep -v gmssl | head -1)
[[ $BASE == gmssl ]] && basepkg=$(ls "$DIST"/base/syapp-*-gmssl-"$ARCH".tar.gz | head -1)
luapkg=$(ls "$DIST"/syapp-lua-*-"$ARCH".tar.gz | head -1)
[[ -f "$basepkg" && -f "$luapkg" ]] || die "missing packages in $DIST"
echo "== 基础包 $(basename "$basepkg")，附加包 $(basename "$luapkg")"

echo "== 安装基础包"
sudo tar -C / -xzf "$basepkg"
sudo useradd -r -s /usr/sbin/nologin syapp 2>/dev/null || true
sudo mkdir -p /run/syapp /var/log/syapp /var/cache/syapp /var/lib/syapp-agent/upgrade /opt/syapp/conf/releases
sudo ip addr add 10.99.0.1/32 dev lo 2>/dev/null || true
VER=$($SY -v 2>&1 | sed -n 's|.*syapp/\([0-9.]*\).*|\1|p')
bin_sha=$(sha256sum "$SY" | cut -c1-64)
check "基础包 -V 带 auth_request" "$($SY -V 2>&1)" "http_auth_request_module"
[[ $BASE == gmssl ]] && check "国密基础包 -V 带 enable-ntls" "$($SY -V 2>&1)" "enable-ntls"
nm -D --defined-only "$SY" | awk '{print $NF}' | sort -u > "$W/exports"   # 写文件再查：管道里 grep -q 提前退出会在 pipefail 下误报
missing=""
for s in EVP_sha256 HMAC CRYPTO_memcmp BIO_new_mem_buf BIO_free PEM_read_bio_PUBKEY EVP_PKEY_free EVP_MD_CTX_new EVP_MD_CTX_free EVP_DigestVerifyInit EVP_DigestVerify ERR_clear_error pcre2_dfa_match_8; do
  grep -qx "$s" "$W/exports" || missing+=" $s"
done
[[ -z "$missing" ]] && ok "基础包导出 etos.jwt 需要的 OpenSSL 函数" || bad "基础包没有导出:$missing"
# 渲染器黄金用例（含规则积木、外部鉴权；国密版另跑国密双证书）过真实基础包的 syapp -t
golden='TestRenderSyappSyntax$|TestRulesSyappSyntax$'
gmenv0=(); [[ $BASE == gmssl ]] && { golden+='|TestRenderGMSyappSyntax$'; gmenv0=(SYAPP_GM_BIN=$SY); }
if out=$(sudo env "PATH=$PATH" CI=1 GOFLAGS=-mod=vendor GOCACHE="$W/gocache" HOME="$W" "${gmenv0[@]}" go test -count=1 -run "$golden" ./internal/render/ 2>&1); then
  ok "渲染器黄金用例（基线、规则积木、外部鉴权$([[ $BASE == gmssl ]] && echo '、国密')）通过真实 syapp -t"
else
  bad "golden syapp -t: $(grep -E 'emerg|FAIL|failed' <<<"$out" | head -3 | tr '\n' ' ')"
fi

echo "== 后端（第二个 syapp 实例，测试替身：业务后端与鉴权服务）"
mkdir -p "$W/backend"
cat > "$W/backend/b.conf" <<EOF
worker_processes 2;
pid $W/backend/pid;
error_log $W/backend/error.log;
events { worker_connections 4096; }
http {
    access_log off;
    server { listen 10.99.0.1:18080; location / { return 200 "backend user=\$http_x_user\n"; } }
    server { listen 10.99.0.1:18081; location = /check { if (\$http_x_token = "good") { return 200; } return 401; } }
}
EOF
sudo "${PIN_BACK[@]}" $SY -p "$W/backend" -c "$W/backend/b.conf" || die "backend did not start"

echo "== 安装 syapp-lua 附加包"
mkdir -p "$W/lua" && tar -C "$W/lua" -xzf "$luapkg"
out=$(sudo bash "$W/lua/install.sh" 2>&1) || die "install.sh failed: $out"
check "附加包安装（自检通过）" "$out" "已安装（$BASE）"
check "模块软链指向本版本" "$(readlink /opt/syapp/modules/lua)" "lua-$VER"
[[ "$(sha256sum "$SY" | cut -c1-64)" == "$bin_sha" ]] && ok "安装附加包不改动基础包二进制" || bad "基础包二进制被改动"

echo "== 生成验收配置（真实渲染器 + 真实 syapp -t）"
HS=$(head -c 64 /dev/urandom | base64 -w0 | tr -d '/+=' | head -c 48)   # 渲染器要求至少 32 个字符
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$W/rs.key" 2>/dev/null
openssl pkey -in "$W/rs.key" -pubout -out "$W/rs.pub"
gmenv=()
if [[ $BASE == gmssl ]]; then
  mkdir -p "$W/gm"
  for u in sign:digitalSignature enc:keyEncipherment,dataEncipherment; do
    k=${u%%:*}; ku=${u#*:}
    openssl genpkey -algorithm SM2 -out "$W/gm/$k.key"
    openssl req -new -x509 -key "$W/gm/$k.key" -sm3 -days 2 -subj /CN=api.test -addext subjectAltName=DNS:api.test -addext "keyUsage=critical,$ku" -out "$W/gm/$k.crt"
  done
  gmenv=(ACCEPT_GM="$W/gm")
fi
render() { # <版本> <是否含 Lua>
  sudo env "PATH=$PATH" GOFLAGS=-mod=vendor GOCACHE="$W/gocache" HOME="$W" ACCEPT_REV="$1" ACCEPT_LUA="$2" \
    ACCEPT_HS_SECRET="$HS" ACCEPT_RS_PUB="$W/rs.pub" "${gmenv[@]}" \
    go test -count=1 -run 'TestAcceptanceConfig$' ./internal/render/ > "$W/render-$1.log" 2>&1
}
render r900100 1 || die "render/syapp -t with Lua rules failed: $(grep -v -E '^(=== RUN|FAIL$|ok )' "$W/render-r900100.log" | head -6 | tr '\n' ' ')"
ok "含 Lua 规则的版本通过真实 syapp -t（加载 ndk / lua 模块）"
sudo ln -sfn releases/r900100 /opt/syapp/conf/current
sudo "${PIN_FRONT[@]}" $SY || die "syapp did not start: $(sudo tail -5 /var/log/syapp/error.log)"   # 热升级的新 master 继承 CPU 绑定
sleep 1

TOK() { python3 scripts/ci/jwtgen.py "$@"; }
H="Host: api.test"

echo "== 原生规则"
check "普通请求经代理到后端" "$(req -H "$H" http://127.0.0.1/)$(body)" "200backend"
check "IP 规则拒绝 10.66.0.0/16" "$(curl -s -o /dev/null -w '%{http_code}' -H "$H" -H 'X-Forwarded-For: 10.66.3.4' http://127.0.0.1/)" "403"
check "UA 规则拒绝 BadBot" "$(req -H "$H" -A 'BadBot/1.0' http://127.0.0.1/)" "403"
check "路径规则跳转并保留路径" "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H "$H" -H 'X-Forwarded-For: 10.251.0.1' http://127.0.0.1/old)" "302 https://new.test/old"
codes=""; for i in $(seq 1 15); do codes+=" $(curl -s -o /dev/null -w '%{http_code}' -H "$H" -H 'X-Forwarded-For: 10.252.0.1' http://127.0.0.1/limited/x)"; done
check "限速：首个请求放行" "${codes:0:4}" "200"
check "限速：超出 5 次/秒返回 429" "$codes" "429"
check "外部鉴权：无凭据 401" "$(req -H "$H" http://127.0.0.1/auth/x)" "401"
check "外部鉴权：鉴权服务放行 200" "$(req -H "$H" -H 'X-Token: good' http://127.0.0.1/auth/x)" "200"
check "外部鉴权只作用于命中的路径" "$(req -H "$H" http://127.0.0.1/other)" "200"
check "ACME 验证路径不受规则影响（BadBot 也能访问）" "$(req -H "$H" -A BadBot http://127.0.0.1/.well-known/acme-challenge/none)" "200"

echo "== JWT（Lua）"
check "HS256：无令牌 401" "$(req -H "$H" http://127.0.0.1/hs/a)" "401"
check "HS256：带 WWW-Authenticate" "$(curl -s -D - -o /dev/null -H "$H" -H 'X-Forwarded-For: 10.253.0.1' http://127.0.0.1/hs/a)" "WWW-Authenticate: Bearer"
t=$(TOK hs "$HS" --iss etos-ci --aud api --sub alice)
check "HS256：有效令牌放行并转发 sub" "$(req -H "$H" -H "Authorization: Bearer $t" -H 'X-User: admin' http://127.0.0.1/hs/a)$(body)" "200backend user=alice"
check "HS256：缓存命中仍正确" "$(req -H "$H" -H "Authorization: Bearer $t" http://127.0.0.1/hs/b)$(body)" "200backend user=alice"
check "HS256：过期令牌 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK hs "$HS" --iss etos-ci --aud api --exp-in -120)" http://127.0.0.1/hs/a)" "401"
check "HS256：签名被篡改 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK hs "$HS" --iss etos-ci --aud api --tamper)" http://127.0.0.1/hs/a)" "401"
check "HS256：iss 不符 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK hs "$HS" --iss other --aud api)" http://127.0.0.1/hs/a)" "401"
check "HS256：alg=none 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK none)" http://127.0.0.1/hs/a)" "401"
check "HS256：伪造 X-User 无令牌 401" "$(req -H "$H" -H 'X-User: admin' http://127.0.0.1/hs/a)" "401"
r=$(TOK rs "$W/rs.key" --sub bob)
check "RS256：有效令牌放行" "$(req -H "$H" -H "Authorization: Bearer $r" http://127.0.0.1/rs/a)" "200"
check "RS256：签名被篡改 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK rs "$W/rs.key" --tamper)" http://127.0.0.1/rs/a)" "401"
check "RS256：算法混淆（用公钥做 HS256 密钥）401" "$(req -H "$H" -H "Authorization: Bearer $(TOK hs "@$W/rs.pub" --alg HS256)" http://127.0.0.1/rs/a)" "401"
check "RS256：过期令牌 401" "$(req -H "$H" -H "Authorization: Bearer $(TOK rs "$W/rs.key" --exp-in -60)" http://127.0.0.1/rs/a)" "401"

echo "== Lua 沙箱（初始化后不能读写文件、执行命令、加载代码或拿到 FFI）"
mkdir -p "$W/sb"
cat > "$W/sb/sb.conf" <<EOF
pid $W/sb/pid;
error_log $W/sb/error.log;
load_module /opt/syapp/modules/lua/ndk_http_module.so;
load_module /opt/syapp/modules/lua/ngx_http_lua_module.so;
events { worker_connections 64; }
http {
    access_log off;
    lua_package_path  "/opt/syapp/modules/lua/lualib/?.lua";
    lua_package_cpath "/opt/syapp/modules/lua/lualib/?.disabled";
    init_by_lua_block { require("etos.init").setup({}) }
    server {
        listen 127.0.0.1:18099;
        location = /jit {
            content_by_lua_block { if jit then local on = jit.status(); ngx.say(tostring(on), " ", jit.arch, " ", jit.version) else ngx.say("no-jit") end }
        }
        location / {
            content_by_lua_block {
                local ok = pcall(require, "ffi")
                ngx.say(type(io), " ", type(os.execute), " ", type(load), " ", type(loadstring), " ", type(debug), " ", tostring(ok))
            }
        }
    }
}
EOF
sudo $SY -p "$W/sb" -c "$W/sb/sb.conf" || bad "sandbox instance did not start"
sleep 0.5
check "沙箱：io / os.execute / load / loadstring / debug 不可用，require ffi 被拒绝" "$(curl -s http://127.0.0.1:18099/)" "nil nil nil nil nil false"
JIT=$(curl -s http://127.0.0.1:18099/jit)
sudo kill "$(cat "$W/sb/pid")" 2>/dev/null

echo "== 标准 HTTPS$([[ $BASE == gmssl ]] && echo ' 与国密 NTLS')"
check "标准客户端 HTTPS（TLS 1.3）" "$(curl -sk -v --resolve api.test:443:127.0.0.1 -H 'X-Forwarded-For: 10.254.0.1' https://api.test/ 2>&1)" "TLSv1.3"
check "HTTPS 下 JWT 同样生效" "$(curl -sk -o /dev/null -w '%{http_code}' --resolve api.test:443:127.0.0.1 -H 'X-Forwarded-For: 10.254.0.2' https://api.test/hs/a)" "401"
if [[ $BASE == gmssl ]]; then
  [[ -x "$DIST/tongsuo" ]] || bad "Tongsuo CLI missing from dist"
  nt=$(echo | timeout 10 "$DIST/tongsuo" s_client -connect 127.0.0.1:443 -servername api.test -enable_ntls -ntls -cipher ECC-SM2-SM4-GCM-SM3 2>&1 || true)
  check "国密 TLCP 握手（装有附加包时）" "$nt" "Cipher is ECC-SM2-SM4-GCM-SM3"
fi

echo "== reload"
out=$(sudo "$WRAPPER" reload 2>&1) || bad "wrapper reload failed: $out"
sleep 1
check "reload 后 JWT 仍然生效（init 重新执行）" "$(req -H "$H" -H "Authorization: Bearer $t" http://127.0.0.1/hs/a)$(body)" "200backend user=alice"
check "reload 后拒绝无令牌请求" "$(req -H "$H" http://127.0.0.1/hs/a)" "401"

echo "== 带附加包的数据面热升级（同一二进制以新版本号打包，验证模块随二进制切换）"
variant=standard; [[ $BASE == gmssl ]] && variant=gmssl
mkpkgs() { # <版本>
  local v=$1
  local d="$W/up-$v"
  mkdir -p "$d/s/sbin" "$d/l"
  sudo cat "$(readlink -f "$SY")" > "$d/s/sbin/syapp"; chmod 755 "$d/s/sbin/syapp"
  echo "$v" > "$d/s/VERSION"; cp docs/NOTICE "$d/s/NOTICE"
  tar -C "$d/s" --owner=0 --group=0 -czf "$d/syapp-$v.tar.gz" sbin/syapp VERSION NOTICE
  tar -C "$d/l" -xzf "$luapkg"
  sed -i "s/\"version\":\"[^\"]*\"/\"version\":\"$v\"/" "$d/l/addon/VERSION"
  tar -C "$d/l" --owner=0 --group=0 -czf "$d/syapp-lua-$v.tar.gz" install.sh uninstall.sh addon
  "$LIC" sign-package -key "$PKG_KEY" -kid "$PKG_KID" -in "$d/syapp-$v.tar.gz" -name syapp -version "$v" -arch "$GOARCH" -variant "$variant" >/dev/null
  "$LIC" sign-package -key "$PKG_KEY" -kid "$PKG_KID" -in "$d/syapp-lua-$v.tar.gz" -name syapp-lua -version "$v" -arch "$GOARCH" -variant lua >/dev/null
  sudo cp "$d"/syapp-*"$v".tar.gz* /var/lib/syapp-agent/upgrade/
}
up2="$VER-2"; up3="$VER-3"
mkpkgs "$up2"
sudo rm -f "/var/lib/syapp-agent/upgrade/syapp-lua-$up2.tar.gz"
out=$(sudo "$WRAPPER" upgrade-start "$up2" 2>&1)
check "缺少附加包时拒绝升级（未改动任何东西）" "$out" "add-on"
check "拒绝后模块软链不变" "$(readlink /opt/syapp/modules/lua)" "lua-$VER"
mkpkgs "$up2"
out=$(sudo "$WRAPPER" upgrade-start "$up2" 2>&1) || bad "upgrade-start $up2 failed: $out"
check "升级后模块软链指向新版本" "$(readlink /opt/syapp/modules/lua)" "lua-$up2"
sleep 1
check "升级中（新 worker）JWT 生效" "$(req -H "$H" -H "Authorization: Bearer $t" http://127.0.0.1/hs/a)$(body)" "200backend user=alice"
out=$(sudo "$WRAPPER" upgrade-commit 2>&1) || bad "upgrade-commit failed: $out"
sleep 1
check "提交后 JWT 生效" "$(req -H "$H" -H "Authorization: Bearer $t" http://127.0.0.1/hs/x)" "200"
mkpkgs "$up3"
out=$(sudo "$WRAPPER" upgrade-start "$up3" 2>&1) || bad "upgrade-start $up3 failed: $out"
out=$(sudo "$WRAPPER" upgrade-abort 2>&1) || bad "upgrade-abort failed: $out"
check "中止后模块软链切回" "$(readlink /opt/syapp/modules/lua)" "lua-$up2"
sleep 1
check "中止后（旧 master 重载配置）JWT 生效" "$(req -H "$H" -H "Authorization: Bearer $t" http://127.0.0.1/hs/y)$(body)" "200backend user=alice"
out=$(sudo "$WRAPPER" reload 2>&1) || bad "reload after abort failed: $out"
check "中止后 reload 正常" "$(req -H "$H" http://127.0.0.1/hs/a)" "401"
check "进程名为 syapp" "$(ps -eo comm | sort -u | grep -i syapp | tr '\n' ' ')" "syapp"
nocheck "没有上游名进程" "$(ps -eo comm,args | grep -vi ' grep' | grep -i 'ngi''nx' || true)" "ngi"

echo "== 性能：三条常用规则（IP 拒绝、UA 拒绝、JWT 校验）下 QPS 下降不超过 15%"
cat > "$W/xff.lua" <<'EOF'
local c = 0
request = function()
  c = c + 1
  wrk.headers["X-Forwarded-For"] = string.format("10.%d.%d.%d", 100 + c % 100, math.random(0, 255), math.random(1, 254))
  return wrk.format(nil)
end
EOF
tb=$(TOK hs "$HS" --exp-in 86400)
bench() { # <host> -> rps（wrk 单线程绑定在独立 CPU 上；前端只有 2 个 CPU，保证瓶颈在被测的 syapp）
  local o; o=$("${PIN_WRK[@]}" wrk -t1 -c64 -d5s -s "$W/xff.lua" -H "Host: $1" -H "Authorization: Bearer $tb" http://127.0.0.1/ 2>&1)
  if grep -q "Non-2xx" <<<"$o"; then echo "::warning title=wrk::non-2xx responses on $1: $(grep Non-2xx <<<"$o")" >&2; fi
  awk '/Requests\/sec/ {v=int($2)} END {print v+0}' <<<"$o"
}
bench bench0.test >/dev/null; bench bench3.test >/dev/null   # 预热
# ABBA 配对：每个比值只比较相隔几秒的两次结果，前后漂移（降频、邻居负载）在块内抵消；取 8 个比值的中位数
ratios=(); raw=""
for blk in 1 2 3 4; do
  a1=$(bench bench0.test); b1=$(bench bench3.test); b2=$(bench bench3.test); a2=$(bench bench0.test)
  raw+=" [$a1 $b1 $b2 $a2]"
  for pr in "$a1 $b1" "$a2 $b2"; do
    set -- $pr
    (( $1 > 0 )) && ratios+=("$(( $2 * 1000 / $1 ))")
  done
done
medr=$(printf '%s\n' "${ratios[@]}" | sort -n | awk '{v[NR]=$1} END {if (NR==0) print 0; else if (NR%2) print v[(NR+1)/2]; else print int((v[NR/2]+v[NR/2+1])/2)}')
drop=$(( 1000 - medr ))
echo "::notice title=perf $BASE/$ARCH::ABBA（无规则 有规则 有规则 无规则）×4：$raw；比值（‰）${ratios[*]}；中位下降 $((drop/10)).$((drop%10))%；jit=${JIT:-?}"
(( ${#ratios[@]} >= 6 && drop <= 150 )) && ok "三条常用规则下 QPS 下降 $((drop/10)).$((drop%10))% ≤ 15%（ABBA 配对中位数）" || bad "QPS 下降 $((drop/10)).$((drop%10))% 超过 15%（$raw）"

echo "== 附加包卸载"
out=$(sudo bash "$W/lua/uninstall.sh" 2>&1) && bad "配置仍使用 Lua 时卸载被允许" || check "配置仍使用 Lua 时拒绝卸载" "$out" "当前配置正在使用 Lua 规则"
render r900101 0 || bad "render without Lua failed"
sudo ln -sfn releases/r900101 /opt/syapp/conf/current
out=$(sudo "$WRAPPER" reload 2>&1) || bad "reload to the no-Lua config failed: $out"
sleep 1
check "切换到不含 Lua 的配置后原生规则仍生效" "$(req -H "$H" -A BadBot http://127.0.0.1/)" "403"
out=$(sudo bash "$W/lua/uninstall.sh" 2>&1) || bad "uninstall failed: $out"
[[ ! -e /opt/syapp/modules/lua ]] && ! ls -d /opt/syapp/modules/lua-* >/dev/null 2>&1 && ok "卸载后模块全部移除" || bad "modules left after uninstall"
out=$(sudo "$WRAPPER" reload 2>&1) || bad "reload after uninstall failed: $out"
sleep 1
check "卸载后 syapp 正常服务" "$(req -H "$H" http://127.0.0.1/)" "200"
check "卸载后（未装附加包）含 Lua 的版本被 syapp -t 拒绝（agent 会回滚）" "$(sudo $SY -t -q -c /opt/syapp/conf/releases/r900100/syapp.conf 2>&1)" "modules/lua"

echo "== 品牌：上游名只允许出现在 /opt/syapp/modules/lua-*/（附加包模块与其 Lua 库）"
out=$(sudo bash "$W/lua/install.sh" 2>&1) || bad "re-install failed: $out"
# NOTICE 等文件中 BSD 许可要求保留的归因句 “Based on NGINX Open Source (BSD-2-Clause)” 不算泄漏
leaks=""
# NOTICE 文件保留 BSD 许可要求的上游版权与归因，属于允许范围
for f in $(sudo grep -rlai 'ngi''nx' /opt/syapp /var/log/syapp 2>/dev/null | grep -v '^/opt/syapp/modules/lua-[^/]*/' | grep -v '/NOTICE$'); do
  sudo grep -ai 'ngi''nx' "$f" | grep -vq 'NGINX Open Source (BSD-2-Clause)' && leaks+=" $f"
done
[[ -z "$leaks" ]] && ok "模块目录之外没有上游名（二进制、配置、日志、脚本）" || bad "上游名出现在：$(echo $leaks)"
leaks=$(sudo grep -rlai 'ngi''nx' /opt/syapp/modules/lua/lualib/etos /opt/syapp/modules/lua/VERSION /opt/syapp/modules/lua/BASE 2>/dev/null || true)
[[ -z "$leaks" ]] && ok "etos 规则库与附加包元数据没有上游名" || bad "上游名出现在：$(echo $leaks)"
[[ "$(sha256sum "$(readlink -f "$SY")" | cut -c1-64)" == "$bin_sha" ]] && ok "基础包二进制自始至终未改变" || bad "base binary changed"

sudo "$SY" -s quit 2>/dev/null; sudo kill "$(cat "$W/backend/pid")" 2>/dev/null
echo "---- $fail failed"
DONE=1
[[ $fail -eq 0 ]] && echo "LUA ACCEPTANCE OK ($BASE/$ARCH)" || exit 1
