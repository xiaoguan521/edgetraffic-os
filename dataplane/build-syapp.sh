#!/usr/bin/env bash
# 构建 syapp 数据面（规格书 4.3）。
#
# 用法：
#   dataplane/build-syapp.sh [-s <上游源码目录>] [-o <输出目录>] [-t openssl|tongsuo]
#
# 不指定 -s 时按 dataplane/UPSTREAM 从官方 GitHub 镜像拉取发布标签，并校验提交哈希。
# 产物：<输出目录>/syapp-<版本>-<架构>.tar.gz，解压到 / 即得到 /opt/syapp 目录树。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/UPSTREAM"

SRC=""; OUT="$PWD/dist"; TLS_LIB="openssl"
while getopts "s:o:t:" opt; do
  case "$opt" in
    s) SRC="$OPTARG" ;;
    o) OUT="$OPTARG" ;;
    t) TLS_LIB="$OPTARG" ;;
    *) exit 2 ;;
  esac
done
mkdir -p "$OUT"; OUT="$(cd "$OUT" && pwd)"   # 转成绝对路径：后面会 cd 进临时目录
[[ "$TLS_LIB" == "openssl" ]] || { echo "TLS_LIB=$TLS_LIB not supported yet (国密构建在二期实现)" >&2; exit 2; }

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
if [[ -z "$SRC" ]]; then
  git -c advice.detachedHead=false clone -q --depth 1 --branch "$UPSTREAM_TAG" "$UPSTREAM_GIT" "$WORK/src"
  got="$(git -C "$WORK/src" rev-parse HEAD)"
  [[ "$got" == "$UPSTREAM_COMMIT" ]] || { echo "upstream commit mismatch: $got != $UPSTREAM_COMMIT" >&2; exit 3; }
else
  cp -a "$SRC" "$WORK/src"
fi
cd "$WORK/src"
grep -q "NGINX_VERSION      \"$UPSTREAM_VERSION\"" src/core/nginx.h || { echo "source is not $UPSTREAM_VERSION" >&2; exit 3; }

# 只允许一个 patch
mapfile -t patches < <(ls "$HERE"/patches/*.patch)
[[ ${#patches[@]} -eq 1 ]] || { echo "expected exactly one patch, found ${#patches[@]}" >&2; exit 2; }
patch -p1 --forward --quiet < "${patches[0]}"
"$HERE/../scripts/rename-scan.sh" "$WORK/src"

./auto/configure \
  --prefix=/opt/syapp \
  --sbin-path=/opt/syapp/sbin/syapp \
  --conf-path=/opt/syapp/conf/current/syapp.conf \
  --pid-path=/run/syapp/syapp.pid \
  --lock-path=/run/syapp/syapp.lock \
  --error-log-path=/var/log/syapp/error.log \
  --http-log-path=/var/log/syapp/access.log \
  --http-client-body-temp-path=/var/cache/syapp/client_temp \
  --http-proxy-temp-path=/var/cache/syapp/proxy_temp \
  --http-fastcgi-temp-path=/var/cache/syapp/fastcgi_temp \
  --http-uwsgi-temp-path=/var/cache/syapp/uwsgi_temp \
  --http-scgi-temp-path=/var/cache/syapp/scgi_temp \
  --user=syapp --group=syapp \
  --with-threads --with-file-aio --with-compat \
  --with-http_ssl_module --with-http_v2_module --with-http_v3_module \
  --with-http_realip_module --with-http_stub_status_module \
  --with-http_gzip_static_module --with-http_sub_module \
  --with-stream --with-stream_ssl_module --with-stream_ssl_preread_module \
  > "$WORK/configure.log"

make -j"$(nproc)" > "$WORK/make.log"
ROOT="$WORK/rootfs"
make install DESTDIR="$ROOT" > /dev/null

# 打包整形：默认配置移到 share/defaults，去掉上游示例文件，conf/current 由 agent 维护为软链
O="$ROOT/opt/syapp"
mkdir -p "$O/share/defaults" "$O/share/doc" "$O/conf/releases"
for f in mime.types fastcgi_params scgi_params uwsgi_params koi-utf koi-win win-utf; do
  [[ -f "$O/conf/current/$f" ]] && mv "$O/conf/current/$f" "$O/share/defaults/$f"
done
rm -rf "$O/conf/current"
cp "$HERE/packaging/html/index.html" "$HERE/packaging/html/50x.html" "$O/html/"
cp "$HERE/../docs/NOTICE" "$O/share/doc/NOTICE"
strip "$O/sbin/syapp"

# 构建后自检
"$O/sbin/syapp" -v 2>&1 | grep -q "^syapp version: syapp/$UPSTREAM_VERSION" || { echo "version check failed" >&2; exit 4; }
if strings "$O/sbin/syapp" | grep -qi 'ngi''nx'; then
  echo "upstream name leaked into binary:" >&2
  strings "$O/sbin/syapp" | grep -i 'ngi''nx' >&2
  exit 4
fi
if find "$ROOT" | grep -qi 'ngi''nx'; then
  echo "upstream name leaked into file names:" >&2; find "$ROOT" | grep -i 'ngi''nx' >&2; exit 4
fi

PKG="$OUT/syapp-$UPSTREAM_VERSION-$(uname -m).tar.gz"
tar -C "$ROOT" -czf "$PKG" opt
echo "BUILD OK: $PKG"
