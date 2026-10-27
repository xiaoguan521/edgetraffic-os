#!/usr/bin/env bash
# 构建 syapp 数据面（规格书 4.3）。用法：./build-syapp.sh <upstream.tar.gz> [openssl|tongsuo]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
TAR="${1:?usage: $0 <upstream.tar.gz> [openssl|tongsuo]}"
TLS_LIB="${2:-openssl}"
source "$HERE/UPSTREAM"

if [[ "$UPSTREAM_SHA256" != "TODO" ]]; then
  echo "$UPSTREAM_SHA256  $TAR" | sha256sum -c -
fi

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
tar -xzf "$TAR" -C "$WORK"
SRC="$(find "$WORK" -mindepth 1 -maxdepth 1 -type d | head -1)"
cd "$SRC"

patches=("$HERE"/patches/*.patch)
if [[ ${#patches[@]} -ne 1 ]]; then
  echo "expected exactly one patch in dataplane/patches, found ${#patches[@]}" >&2
  exit 2
fi
patch -p1 --forward < "${patches[0]}"

./configure \
  --prefix=/opt/syapp \
  --sbin-path=/opt/syapp/sbin/syapp \
  --conf-path=/opt/syapp/conf/current/syapp.conf \
  --pid-path=/run/syapp/syapp.pid \
  --lock-path=/run/syapp/syapp.lock \
  --error-log-path=/var/log/syapp/error.log \
  --http-log-path=/var/log/syapp/access.log \
  --http-client-body-temp-path=/var/cache/syapp/client_temp \
  --http-proxy-temp-path=/var/cache/syapp/proxy_temp \
  --user=syapp --group=syapp \
  --with-threads --with-file-aio --with-compat \
  --with-http_ssl_module --with-http_v2_module --with-http_v3_module \
  --with-http_realip_module --with-http_stub_status_module \
  --with-http_gzip_static_module --with-http_sub_module \
  --with-stream --with-stream_ssl_module --with-stream_ssl_preread_module \
  ${TLS_SRC:+--with-openssl="$TLS_SRC"}

make -j"$(nproc)"
make install DESTDIR="$SRC/rootfs"
"$SRC/rootfs/opt/syapp/sbin/syapp" -v 2>&1 | grep -q '^syapp version' || { echo "rename check failed" >&2; exit 3; }
echo "BUILD OK: $SRC/rootfs  (TLS_LIB=$TLS_LIB)"
