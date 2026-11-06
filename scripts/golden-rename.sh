#!/usr/bin/env bash
# 改名黄金验收（规格书 4.6）。在已安装 syapp 并加载了基线配置的机器上运行。
#
# 用法：scripts/golden-rename.sh [syapp 路径] [站点域名]
#   站点域名需在当前配置中存在（基线示例为 www.example.com），HTTPS 证书可为自签。
set -uo pipefail
BIN="${1:-/opt/syapp/sbin/syapp}"
SITE="${2:-www.example.com}"
name='ngi''nx'   # 避免本脚本自身被品牌门禁误报
export NO_PROXY='*' no_proxy='*'   # 只访问本机，绕过任何代理
C=(curl -s --max-time 5)
fail=0; skip=0
# 子 shell 关闭 pipefail：grep -q 提前退出会让上游命令收到 SIGPIPE
check() { if (set +o pipefail; eval "$2") >/dev/null 2>&1; then echo "ok   $1"; else echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=golden-rename::FAIL $1"; fail=$((fail+1)); fi; }
skipif() { echo "skip $1 ($2)"; skip=$((skip+1)); }

check "-v 输出 syapp"                "\"$BIN\" -v 2>&1 | grep -q '^syapp version: syapp/'"
check "-h 用法为 syapp 且无上游名"     "\"$BIN\" -h 2>&1 | grep -q '^Usage: syapp' && ! \"$BIN\" -h 2>&1 | grep -qi $name"
check "-t 输出前缀为 syapp"           "\"$BIN\" -t 2>&1 | grep -q '^syapp: '"
check "-V 配置路径"                   "\"$BIN\" -V 2>&1 | grep -q -- '--conf-path=/opt/syapp/conf/current/syapp.conf'"
check "ps -C syapp"                  "ps -C syapp -o comm= | grep -q syapp"
check "master 进程标题"               "ps -eo args | grep -q '^syapp: master process'"
check "worker 进程标题"               "ps -eo args | grep -q '^syapp: worker process'"
check "master 与 worker 都不以 root 运行" "! ps -C syapp -o user= | grep -qx root"
if command -v systemctl >/dev/null && [[ -d /run/systemd/system ]]; then
  check "systemd 单元 syapp.service"  "systemctl is-active syapp.service"
else
  skipif "systemd 单元 syapp.service" "本机无 systemd"
fi
check "HTTP/1.1 Server 头"            "${C[*]} -I -H 'Host: $SITE' http://127.0.0.1/ | grep -qi '^server: syapp'"
check "HTTP/2 server 头"              "${C[*]} -k -I --http2 --resolve $SITE:443:127.0.0.1 https://$SITE/ | grep -qi '^server: syapp'"
check "默认错误页"                    "${C[*]} -k --resolve $SITE:443:127.0.0.1 https://$SITE/__no_such_page__ | grep -q '<center>syapp</center>'"
check "未知 Host 直接断开"            "! ${C[*]} -o /dev/null http://127.0.0.1/"
check "二进制无上游名"                "! strings \"$BIN\" | grep -qi $name"
check "安装目录无上游名"              "! find /opt/syapp | grep -qi $name"

echo "---- $fail failed, $skip skipped"
[[ $fail -eq 0 ]] && echo "RENAME GOLDEN OK" || exit 1
