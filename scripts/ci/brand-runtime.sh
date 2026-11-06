#!/usr/bin/env bash
# 品牌的运行时检查（设计 0001 第 4 节 BR-01 / BR-02、D7）：解压数据面包，在临时目录中启动，检查对外可见的名字。
#
#   scripts/ci/brand-runtime.sh <数据面 tar.gz> <品牌 ID> <显示名>
#   例：scripts/ci/brand-runtime.sh dist/syapp-1.30.6-x86_64.tar.gz syapp syapp
#       scripts/ci/brand-runtime.sh dist/syedge-1.30.6-x86_64.tar.gz syedge SyEdge
#
# 检查：-v / -t 输出、master 与 worker 进程标题、HTTP/1.1 与 HTTP/2 的 Server 头、默认错误页、
# $<品牌>_version 变量；二进制中没有上游名，定制品牌时也没有默认品牌名
# （HTTP/2 的 Server 头是 HPACK 哈夫曼编码，strings 扫描看不到，只能这样实测）。
# 需要 root（解压到编译期前缀 /opt/<品牌>）、curl（支持 HTTP/2）、openssl。
set -uo pipefail
TGZ=$(realpath "$1"); ID=$2; PROC=$3; DEFAULT=syapp
[[ $EUID -eq 0 ]] || { echo "需要 root" >&2; exit 2; }
export NO_PROXY='*' no_proxy='*'
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=brand-runtime ($ID)::$1"; fail=$((fail+1)); }
W=$(mktemp -d); chmod 755 "$W"
tar -C / -xzf "$TGZ"
BIN=/opt/$ID/sbin/$ID
[[ -x "$BIN" ]] || { echo "包中没有 $BIN" >&2; exit 2; }
id nobody >/dev/null 2>&1 || useradd -r nobody
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=brand.test \
  -addext subjectAltName=DNS:brand.test -keyout "$W/k.pem" -out "$W/c.pem" 2>/dev/null
P1=18991; P2=18992
cat > "$W/t.conf" <<EOF
user nobody $(id -gn nobody);
worker_processes 1;
pid $W/master.pid;
error_log $W/error.log;
events { worker_connections 64; }
http {
    server_tokens off;
    access_log off;
    client_body_temp_path $W/body; proxy_temp_path $W/proxy; fastcgi_temp_path $W/fcgi; uwsgi_temp_path $W/uwsgi; scgi_temp_path $W/scgi;
    server {
        listen 127.0.0.1:$P1;
        listen 127.0.0.1:$P2 ssl;
        http2 on;
        ssl_certificate $W/c.pem;
        ssl_certificate_key $W/k.pem;
        location = /v { return 200 "\$${ID}_version\n"; }
    }
}
EOF
out=$("$BIN" -v 2>&1); [[ "$out" == "$PROC version: $PROC/"* ]] && ok "-v：$out" || bad "-v 输出 $out"
out=$("$BIN" -t -c "$W/t.conf" -e "$W/error.log" 2>&1)
[[ "$out" == "$PROC: "* && "$out" == *"test is successful"* ]] && ok "-t 输出以 \"$PROC: \" 开头" || bad "-t 输出 $out"
"$BIN" -c "$W/t.conf" -e "$W/error.log" || { bad "启动失败：$(tail -3 "$W/error.log")"; exit 1; }
trap '"$BIN" -c "$W/t.conf" -e "$W/error.log" -s quit 2>/dev/null; sleep 0.5; rm -rf "$W"' EXIT
sleep 0.5
m=$(cat "$W/master.pid")
[[ "$(tr '\0' ' ' < "/proc/$m/cmdline")" == "$PROC: master process "* ]] && ok "master 进程标题 \"$PROC: master process\"" || bad "master 标题 $(tr '\0' ' ' < "/proc/$m/cmdline")"
w=$(pgrep -P "$m" | head -1)
[[ "$(tr '\0' ' ' < "/proc/$w/cmdline")" == "$PROC: worker process"* ]] && ok "worker 进程标题" || bad "worker 标题 $(tr '\0' ' ' < "/proc/$w/cmdline")"
h=$(curl -s -o /dev/null -D - "http://127.0.0.1:$P1/v" | tr -d '\r' | grep -i '^server:')
[[ "$h" == "Server: $PROC" ]] && ok "HTTP/1.1 $h" || bad "HTTP/1.1 Server 头为 \"$h\"，期望 \"Server: $PROC\""
h=$(curl -sk --http2 -o /dev/null -D - --resolve brand.test:$P2:127.0.0.1 "https://brand.test:$P2/v" | tr -d '\r')
[[ "$h" == HTTP/2* ]] || bad "没有协商到 HTTP/2"
h=$(grep -i '^server:' <<<"$h")
[[ "$h" == "server: $PROC" ]] && ok "HTTP/2 $h" || bad "HTTP/2 server 头为 \"$h\"，期望 \"server: $PROC\""
b=$(curl -s "http://127.0.0.1:$P1/missing")
[[ "$b" == *"<center>$PROC</center>"* ]] && ok "默认错误页显示 $PROC" || bad "错误页：$b"
v=$(curl -s "http://127.0.0.1:$P1/v")
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] && ok "\$${ID}_version = $v" || bad "\$${ID}_version 返回 \"$v\""
if strings "$BIN" | grep -qi 'ngi''nx'; then bad "二进制中有上游名"; else ok "二进制中没有上游名"; fi
if [[ "$ID" != "$DEFAULT" ]]; then
  if strings "$BIN" | grep -qi "$DEFAULT"; then bad "二进制中残留默认品牌名"; else ok "二进制中没有默认品牌名 $DEFAULT"; fi
  if grep -rqi "$DEFAULT" "/opt/$ID" 2>/dev/null; then bad "/opt/$ID 中残留默认品牌名"; else ok "/opt/$ID 中没有默认品牌名"; fi
fi
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "BRAND RUNTIME OK ($ID / $PROC)" || exit 1
