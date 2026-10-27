#!/usr/bin/env bash
# 在上游源码中扫描用户可见的产品名字串，发现改名 patch 未覆盖的新位置（规格书 4.6）。
# 用法：scripts/rename-scan.sh <已打 patch 的上游源码目录>
set -euo pipefail
SRC="${1:?usage: $0 <patched-upstream-src>}"
cd "$SRC/src"
pattern='"nginx|nginx: |<center>nginx|NGINX_CTRL|"NGINX"'
hits=$(grep -rnE "$pattern" --include='*.c' --include='*.h' . \
  | grep -v -E '^\./os/win32/|/perl/|/quic/bpf/' || true)
if [[ -n "$hits" ]]; then
  echo "rename-scan FAILED: uncovered strings:"
  echo "$hits"
  exit 1
fi
echo "rename-scan OK"
