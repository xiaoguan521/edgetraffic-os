#!/usr/bin/env bash
# 改名黄金验收（规格书 4.6），在安装了 syapp 软件包并启动服务的测试机上运行。
set -uo pipefail
BIN="${1:-/opt/syapp/sbin/syapp}"
HOST="${2:-127.0.0.1}"
fail=0
check() { if eval "$2" >/dev/null 2>&1; then echo "ok   $1"; else echo "FAIL $1"; fail=$((fail+1)); fi; }
brand='ngi''nx'   # 避免本脚本自身被品牌门禁误报

check "-v 输出 syapp"            "\"$BIN\" -v 2>&1 | grep -q '^syapp version'"
check "-h 无上游名"              "! \"$BIN\" -h 2>&1 | grep -qi $brand"
check "-V conf-path"            "\"$BIN\" -V 2>&1 | grep -q -- '--conf-path=/opt/syapp/conf/current/syapp.conf'"
check "ps -C syapp"             "ps -C syapp -o comm= | grep -q syapp"
check "master 进程标题"          "ps -eo args | grep -q '^syapp: master process'"
check "worker 进程标题"          "ps -eo args | grep -q '^syapp: worker process'"
check "systemd 单元"             "systemctl is-active syapp.service"
check "Server 头"               "curl -sI http://$HOST/ | grep -qi '^server: syapp'"
check "404 错误页"               "! curl -s http://$HOST/__no_such_page__ | grep -qi $brand"
check "strings 泄漏审计"         "! strings \"$BIN\" | grep -qiE \"$brand version|usage: $brand\""
[[ $fail -eq 0 ]] && echo "RENAME GOLDEN OK" || { echo "$fail check(s) failed"; exit 1; }
