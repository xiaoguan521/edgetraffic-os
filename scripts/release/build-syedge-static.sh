#!/usr/bin/env bash
# 在 manylinux2014 容器（CentOS 7 基础，glibc 2.17）中静态构建 syedge，产物可在 glibc 2.17 及以上的发行版运行。
# 用法：scripts/release/build-syedge-static.sh [输出目录，默认 dist/syedge]（需要 Docker，按本机架构构建）
# TLS_LIB=tongsuo 时构建国密变体（syedge-<版本>-gmssl-<架构>.tar.gz）。
# LUA=1 时构建 syedge-lua 附加包（dataplane/build-syedge-lua.sh，同时产出两种基础包到 <输出目录>/base）。
# SYEDGE_PREFIX 可覆盖数据面与 Go 组件的编译期默认安装根目录。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="${1:-dist/syedge}"
SYEDGE_PREFIX="${SYEDGE_PREFIX:-/opt/syedge}"
case "$(uname -m)" in
  x86_64) img=quay.io/pypa/manylinux2014_x86_64 ;;
  aarch64) img=quay.io/pypa/manylinux2014_aarch64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 2 ;;
esac
# 输出目录先在宿主机上以当前用户创建：否则容器（root）会把上层的 dist/ 也建成 root 所有，
# 后续以普通用户运行的打包步骤无法在 dist/ 下创建目录。
mkdir -p "$ROOT/$OUT"
docker run --rm -v "$ROOT:/src" -w /src -e OUT="$OUT" -e SYEDGE_PREFIX="$SYEDGE_PREFIX" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" -e TLS_LIB="${TLS_LIB:-openssl}" -e LUA="${LUA:-}" "$img" bash -c '
  set -euo pipefail
  yum install -y -q perl-core git patch >/dev/null
  if [[ -n "$LUA" ]]; then
    MAX_GLIBC=2.17 dataplane/build-syedge-lua.sh -o "$OUT"
  else
    MAX_GLIBC=2.17 dataplane/build-syedge.sh -S -t "$TLS_LIB" -o "$OUT"
  fi
  chown -R "$HOST_UID:$HOST_GID" "$OUT"
'
ls -l "$ROOT/$OUT"
