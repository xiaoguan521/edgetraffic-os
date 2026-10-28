#!/usr/bin/env bash
# 在 manylinux2014 容器（CentOS 7 基础，glibc 2.17）中静态构建 syapp，产物可在 glibc 2.17 及以上的发行版运行。
# 用法：scripts/release/build-syapp-static.sh [输出目录，默认 dist/syapp]（需要 Docker，按本机架构构建）
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="${1:-dist/syapp}"
case "$(uname -m)" in
  x86_64) img=quay.io/pypa/manylinux2014_x86_64 ;;
  aarch64) img=quay.io/pypa/manylinux2014_aarch64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 2 ;;
esac
docker run --rm -v "$ROOT:/src" -w /src -e OUT="$OUT" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" "$img" bash -c '
  set -euo pipefail
  yum install -y -q perl-core git patch >/dev/null
  MAX_GLIBC=2.17 dataplane/build-syapp.sh -S -o "$OUT"
  chown -R "$HOST_UID:$HOST_GID" "$OUT"
'
ls -l "$ROOT/$OUT"
