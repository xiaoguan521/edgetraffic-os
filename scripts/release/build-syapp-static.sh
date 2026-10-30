#!/usr/bin/env bash
# 在 manylinux2014 容器（CentOS 7 基础，glibc 2.17）中静态构建 syapp，产物可在 glibc 2.17 及以上的发行版运行。
# 用法：scripts/release/build-syapp-static.sh [输出目录，默认 dist/syapp]（需要 Docker，按本机架构构建）
# TLS_LIB=tongsuo 时构建国密变体（syapp-<版本>-gmssl-<架构>.tar.gz）。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="${1:-dist/syapp}"
case "$(uname -m)" in
  x86_64) img=quay.io/pypa/manylinux2014_x86_64 ;;
  aarch64) img=quay.io/pypa/manylinux2014_aarch64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 2 ;;
esac
# 输出目录先在宿主机上以当前用户创建：否则容器（root）会把上层的 dist/ 也建成 root 所有，
# 后续以普通用户运行的打包步骤无法在 dist/ 下创建目录。
mkdir -p "$ROOT/$OUT"
docker run --rm -v "$ROOT:/src" -w /src -e OUT="$OUT" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" -e TLS_LIB="${TLS_LIB:-openssl}" "$img" bash -c '
  set -euo pipefail
  yum install -y -q perl-core git patch >/dev/null
  MAX_GLIBC=2.17 dataplane/build-syapp.sh -S -t "$TLS_LIB" -o "$OUT"
  chown -R "$HOST_UID:$HOST_GID" "$OUT"
'
ls -l "$ROOT/$OUT"
