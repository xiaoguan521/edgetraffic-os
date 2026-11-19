#!/usr/bin/env bash
# 国密变体验收（P8）：安装 dist-gm 中的国密版 syedge（铜锁 NTLS 构建），
#   1) 用真实 syedge -t 校验渲染器生成的国密配置（两个国密站点共用 443）；
#   2) 启动 syedge，用铜锁 s_client 做国密 TLCP 握手（ECC-SM2-SM4），按 SNI 命中各站点自己的双证书；
#   3) 同一端口的标准 TLS 1.3 仍然正常（不支持国密的客户端走标准证书）。
# 用法：scripts/ci/gm-handshake.sh [dist-gm 目录]   （需要 sudo 与 systemd）
set -euo pipefail
trap 'echo "::error title=gm-handshake::exit $? at: $BASH_COMMAND"' ERR
DIST=$(realpath "${1:-dist-gm}")
CLI="$DIST/tongsuo"
W=$(mktemp -d)
SY=/opt/syedge/sbin/syedge

echo "== 安装国密版 syedge"
sudo tar -C / -xzf "$DIST"/syedge-*-gmssl-*.tar.gz
$SY -V 2>&1 | tee "$W/version"
grep -q "(gmssl)" "$W/version"
grep -q "enable-ntls" "$W/version"
if strings "$SY" | grep -qi 'ngi''nx'; then echo "::error::upstream name in GM binary"; exit 1; fi
sudo useradd -r -s /usr/sbin/nologin syedge 2>/dev/null || true
sudo mkdir -p /opt/syedge/run /opt/syedge/logs /opt/syedge/cache

echo "== 签发真实的 SM2 签名 / 加密证书（SM2-with-SM3）"
for u in sign:digitalSignature enc:keyEncipherment,dataEncipherment; do
  n=${u%%:*}; ku=${u#*:}
  openssl genpkey -algorithm SM2 -out "$W/$n.key"
  openssl req -new -x509 -key "$W/$n.key" -sm3 -days 2 -subj /CN=shop.example.com \
    -addext subjectAltName=DNS:shop.example.com -addext "keyUsage=critical,$ku" -out "$W/$n.crt"
done

echo "== 渲染器国密配置通过真实 syedge -t（版本 r900001 保留用于启动）"
sudo env "PATH=$PATH" GOFLAGS=-mod=vendor GOCACHE="$W/gocache" HOME="$W" \
  SYEDGE_GM_BIN=$SY GM_KEEP=1 \
  GM_SIGN_CERT="$W/sign.crt" GM_SIGN_KEY="$W/sign.key" GM_ENC_CERT="$W/enc.crt" GM_ENC_KEY="$W/enc.key" \
  go test -count=1 -v -run 'TestRenderGMSyedgeSyntax$' ./internal/render/ | tail -5
grep -q "Enable_ntls" /opt/syedge/conf/releases/r900001/conf.d/site-shop.conf

echo "== 启动"
sudo mkdir -p /srv/syedge
sudo ln -sfn releases/r900001 /opt/syedge/conf/current
# 运行时不使用 root（设计 0001 D5）：单元以 syedge 运行；运行时目录归 syedge，版本目录与私钥按 agent 的方式组可读（0640，组 syedge）
id syedge >/dev/null 2>&1 || sudo useradd -r -s /usr/sbin/nologin syedge
sudo chown -R syedge:syedge /opt/syedge/run /opt/syedge/logs /opt/syedge/cache
sudo chgrp -R syedge /opt/syedge/conf && sudo chmod -R g+rX /opt/syedge/conf
sudo find /opt/syedge/conf -name '*.key' -exec chmod 0640 {} +
sudo cp dataplane/packaging/systemd/syedge.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start syedge || { sudo journalctl -u syedge --no-pager | tail -20 | sed 's/^/::error::/'; exit 1; }
sleep 1

ntls() { # <SNI> <套件>
  echo | timeout 10 "$CLI" s_client -connect 127.0.0.1:443 -servername "$1" -enable_ntls -ntls -cipher "$2" 2>&1 || true
}
fail=0
check() { if grep -q "$3" <<<"$2"; then echo "ok   $1"; else echo "::error title=gm-handshake::$1"; echo "$2" | tail -25; fail=1; fi; }

out=$(ntls shop.example.com ECC-SM2-SM4-GCM-SM3)
check "国密 TLCP 握手（ECC-SM2-SM4-GCM-SM3）" "$out" "Cipher is ECC-SM2-SM4-GCM-SM3"
check "国密握手返回站点 shop 的 SM2 证书" "$out" "CN *= *shop.example.com"
out=$(ntls shop.example.com ECC-SM2-SM4-CBC-SM3)
check "国密 TLCP 握手（ECC-SM2-SM4-CBC-SM3）" "$out" "Cipher is ECC-SM2-SM4-CBC-SM3"
out=$(ntls blog.example.com ECC-SM2-SM4-GCM-SM3)
check "按 SNI 切换到站点 blog 的国密双证书" "$out" "CN *= *blog.example.com"

code=$(curl -sk --resolve shop.example.com:443:127.0.0.1 -o /dev/null -w '%{http_code}' https://shop.example.com/)
check "同一端口标准 HTTPS 正常" "$code" "200"
check "标准客户端协商 TLS 1.3" "$(curl -sk -v --resolve shop.example.com:443:127.0.0.1 -o /dev/null https://shop.example.com/ 2>&1)" "TLSv1.3"

echo "-- 无 SNI 的国密握手（默认 server 拒绝握手，与标准 TLS 一致，仅记录）"
echo | timeout 10 "$CLI" s_client -connect 127.0.0.1:443 -enable_ntls -ntls 2>&1 | grep -E "alert|Cipher is" | head -2 || true

sudo systemctl stop syedge
[[ $fail -eq 0 ]] && echo "GM HANDSHAKE OK" || exit 1
