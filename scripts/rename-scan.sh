#!/usr/bin/env bash
# 在已打 patch 的上游源码中扫描用户可见的产品名字串，发现改名 patch 未覆盖的位置（规格书 4.6）。
# 用法：scripts/rename-scan.sh <已打 patch 的上游源码目录>
# 不检查：Windows 端、perl 模块、QUIC eBPF 程序（均不在构建范围内）。
set -euo pipefail
SRC="${1:?usage: $0 <patched-upstream-src>}"
cd "$SRC/src"
name='ngi''nx'
# 1) 任何字符串字面量中出现的小写产品名；2) 大写环境变量名
hits=$( { grep -rnE "\"[^\"]*${name}[^\"]*\"" --include='*.c' --include='*.h' . || true; \
          grep -rnE "\"${name^^}[A-Z_]*\"" --include='*.c' --include='*.h' . || true; } \
  | grep -v -E '^\./os/win32/|/perl/|/quic/bpf/|#include' \
  | grep -v -E "ngx_(http|stream)_variable_${name}_version" || true)
if [[ -n "$hits" ]]; then
  echo "rename-scan FAILED: uncovered strings:"
  echo "$hits"
  exit 1
fi
echo "rename-scan OK"
