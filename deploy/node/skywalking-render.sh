#!/usr/bin/env bash
# 用三个值生成 conf.d/10-skywalking.conf：只替换模板里固定的三行（OAP_ADDRESS / SERVICE_NAME / INSTANCE_NAME），
# 值经严格校验，不接受任何会改变 Lua 语义的字符。随整合包分发，由 install.sh 调用，也可单独运行。
#   用法：skywalking-render.sh <模板> <OAP 地址> <服务名> <实例名> <输出文件>
set -euo pipefail
die() { echo "skywalking: $*" >&2; exit 1; }
[[ $# -eq 5 ]] || die "用法：$0 <模板> <OAP 地址> <服务名> <实例名> <输出文件>"
TPL=$1 OAP=$2 SVC=$3 INST=$4 OUT=$5
[[ -f "$TPL" ]] || die "找不到模板 $TPL"

# OAP：只接受 http(s)://主机:端口，不带路径、用户信息与特殊字符
[[ "$OAP" =~ ^https?://[A-Za-z0-9._-]+(:([0-9]{1,5}))?$ ]] || die "OAP 地址必须形如 http://主机:端口（不含路径与特殊字符）：$OAP"
if [[ -n "${BASH_REMATCH[2]}" ]]; then
  (( 10#${BASH_REMATCH[2]} >= 1 && 10#${BASH_REMATCH[2]} <= 65535 )) || die "OAP 端口无效：$OAP"
fi
[[ "$OAP" != *192.0.2.* ]] || die "OAP 地址仍是示例地址：$OAP"
[[ "$SVC" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "服务名只能含字母、数字、点、下划线、连字符，最长 64：$SVC"
[[ "$INST" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "实例名只能含字母、数字、点、下划线、连字符，最长 64：$INST"

out_dir=$(dirname "$OUT")
[[ -d "$out_dir" ]] || die "输出目录不存在：$out_dir"
tmp=$(mktemp "$out_dir/.skywalking.XXXXXX")
trap 'rm -f "$tmp"' EXIT
sed -E \
  -e "s|^([[:space:]]*local OAP_ADDRESS[[:space:]]*=[[:space:]]*)\"[^\"]*\"|\\1\"$OAP\"|" \
  -e "s|^([[:space:]]*local SERVICE_NAME[[:space:]]*=[[:space:]]*)\"[^\"]*\"|\\1\"$SVC\"|" \
  -e "s|^([[:space:]]*local INSTANCE_NAME[[:space:]]*=[[:space:]]*)\"[^\"]*\"|\\1\"$INST\"|" "$TPL" > "$tmp"
# 三行都必须替换成功，且各只出现一次
for want in "local OAP_ADDRESS   = \"$OAP\"" "local SERVICE_NAME  = \"$SVC\"" "local INSTANCE_NAME = \"$INST\""; do
  [[ $(grep -cF -- "$want" "$tmp") -eq 1 ]] || die "模板里没有找到可替换的行：${want%%=*}"
done
chmod 0644 "$tmp"
mv -f "$tmp" "$OUT"
trap - EXIT
