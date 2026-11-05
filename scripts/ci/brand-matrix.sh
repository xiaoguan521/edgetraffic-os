#!/usr/bin/env bash
# 定制品牌构建检查（设计 0001 第 4.4 节），在本地运行（make brand-matrix），不占用 GitHub Actions。
#
# 用一个测试品牌完整走一遍：生成改名 patch 并核对 → 以 -tags brand 构建全部 Go 程序并扫描二进制 →
# 用替身数据面打节点发布包（真实的改名构建与 syapp -t 由 ci 工作流的 dataplane 任务覆盖）。
# 任何位置残留默认品牌名（Go 模块路径与协议 / 数据库字段名除外）即失败。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
ID="${1:-sytest}"; PROC="${2:-SyTest}"
DEFAULT_BRAND=syapp
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
export GOFLAGS=-mod=vendor CGO_ENABLED=0

echo "== 1/3 改名 patch：默认品牌与已提交的 patch 逐字节相同，测试品牌不残留默认名"
go test -count=1 ./dataplane/gen-rename-patch/
go run ./dataplane/gen-rename-patch -id "$ID" -proc "$PROC" -o "$W/rename.patch"
if grep '^+' "$W/rename.patch" | grep -v '^+++' | grep -qi "$DEFAULT_BRAND"; then
  echo "default brand left in the generated patch" >&2; exit 1
fi

echo "== 2/3 Go 程序（-tags brand）"
BP=github.com/xiaoguan521/edgetraffic-os/internal/brand
for c in cmd/*/; do
  c=${c%/}; out="$W/bin/$(basename "$c")"
  go build -tags brand -trimpath -ldflags "-s -w -X $BP.id=$ID -X $BP.proc=$PROC -X '$BP.product=$PROC Gateway'" -o "$out" "./$c"
  if strings "$out" | grep -i "$DEFAULT_BRAND" | grep -v 'github.com/xiaoguan521/edgetraffic-os/' \
      | grep -qvi "${DEFAULT_BRAND}_\?version\|${DEFAULT_BRAND}_\?sha256"; then
    echo "default brand left in $c:" >&2
    strings "$out" | grep -i "$DEFAULT_BRAND" | grep -v 'github.com/xiaoguan521/edgetraffic-os/' | grep -vi "${DEFAULT_BRAND}_\?version\|${DEFAULT_BRAND}_\?sha256" | head >&2
    exit 1
  fi
  echo "  $c OK"
done

echo "== 3/3 节点发布包与 user 模式节点包（替身数据面）"
mkdir -p "$W/dp/opt/$ID/sbin" "$W/out"
printf '#!/bin/sh\necho "%s version: %s/0.0.0"\n' "$PROC" "$PROC" > "$W/dp/opt/$ID/sbin/$ID"
chmod +x "$W/dp/opt/$ID/sbin/$ID"
tar -C "$W/dp" -czf "$W/dp.tgz" opt
printf 'BRAND_ID=%s\nBRAND_PROC=%s\nBRAND_PRODUCT="%s Gateway"\n' "$ID" "$PROC" "$PROC" > "$W/brand.env"
for t in node node-user; do BRAND_ENV="$W/brand.env" OUT="$W/out" scripts/release/package.sh "$t" amd64 "$W/dp.tgz"; done
echo "brand-matrix OK ($ID / $PROC)"
