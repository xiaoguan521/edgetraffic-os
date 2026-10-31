#!/usr/bin/env bash
# 构建 syapp 数据面（规格书 4.3）。
#
# 用法：
#   dataplane/build-syapp.sh [-s <上游源码目录>] [-o <输出目录>] [-t openssl|tongsuo] [-S]
#
# 不指定 -s 时按 dataplane/UPSTREAM 从官方 GitHub 镜像拉取发布标签，并校验提交哈希。
# -t tongsuo：国密变体。铜锁按 dataplane/UPSTREAM 拉取发布标签并校验提交哈希，开启 NTLS 后静态链接（隐含 -S）；
#     configure 带 --build=gmssl，syapp -V 显示 "(gmssl)" 与 enable-ntls，控制台据此识别国密节点。
#     KEEP_TLS_CLI=<路径> 时把铜锁的命令行工具复制出来（CI 用它做国密握手测试，不进安装包）。
# LUA_MODULES_OUT=<目录>：基础包打包后，用完全相同的 configure 参数再编译 Lua 附加包的两个动态模块
#     （ngx_devel_kit、lua 模块，需要 NDK_SRC / LUA_MODULE_SRC / LUAJIT_INC / LUAJIT_LIB），只输出 .so，
#     并核对模块需要的每个符号都由基础包或 ngx_devel_kit 导出（见 dataplane/lua/base-exports.txt）。由 build-syapp-lua.sh 调用。
# -S：OpenSSL、PCRE2、zlib 按 dataplane/DEPS 下载源码并静态链接，二进制只依赖 glibc。
#     在 glibc 2.17 的构建环境（manylinux2014）中执行，即可覆盖 CentOS 7、麒麟 V10、统信 UOS、openEuler 与较新的发行版。
#     DEPS_CACHE 指向已下载源码包的目录时优先使用缓存；MAX_GLIBC 设置后校验二进制不依赖更高版本的 glibc 符号。
# 产物：<输出目录>/syapp-<版本>-<架构>.tar.gz（国密变体为 syapp-<版本>-gmssl-<架构>.tar.gz），解压到 / 即得到 /opt/syapp 目录树。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/UPSTREAM"

SRC=""; OUT="$PWD/dist"; TLS_LIB="openssl"; STATIC=0
while getopts "s:o:t:S" opt; do
  case "$opt" in
    s) SRC="$OPTARG" ;;
    o) OUT="$OPTARG" ;;
    t) TLS_LIB="$OPTARG" ;;
    S) STATIC=1 ;;
    *) exit 2 ;;
  esac
done
mkdir -p "$OUT"; OUT="$(cd "$OUT" && pwd)"   # 转成绝对路径：后面会 cd 进临时目录
case "$TLS_LIB" in
  openssl) ;;
  tongsuo) STATIC=1 ;;
  *) echo "TLS_LIB must be openssl or tongsuo" >&2; exit 2 ;;
esac

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

# 静态依赖：下载（或取缓存）→ 校验 sha256 → 解压，交给 configure 一起编译
DEP_OPTS=()
if [[ $STATIC -eq 1 ]]; then
  # shellcheck disable=SC1091
  source "$HERE/DEPS"
  mkdir -p "$WORK/deps"
  fetch_dep() { # name url sha256 -> 解压后的目录
    local name=$1 url=$2 sum=$3 file
    file="${DEPS_CACHE:-$WORK/deps}/$(basename "$url")"
    if [[ ! -f "$file" ]]; then
      mkdir -p "$(dirname "$file")"
      curl -fsSL --retry 3 -o "$file" "$url"
    fi
    echo "$sum  $file" | sha256sum -c --quiet - || { echo "$name: sha256 mismatch for $file" >&2; exit 3; }
    tar -C "$WORK/deps" -xzf "$file"
    echo "$WORK/deps/$(tar -tzf "$file" | head -1 | cut -d/ -f1)"
  }
  PCRE2_DIR=$(fetch_dep pcre2 "$PCRE2_URL" "$PCRE2_SHA256")
  ZLIB_DIR=$(fetch_dep zlib "$ZLIB_URL" "$ZLIB_SHA256")
  if [[ "$TLS_LIB" == "tongsuo" ]]; then
    TLS_DIR="$WORK/deps/Tongsuo"
    git -c advice.detachedHead=false clone -q --depth 1 --branch "$TONGSUO_TAG" "$TONGSUO_GIT" "$TLS_DIR"
    got="$(git -C "$TLS_DIR" rev-parse HEAD)"
    [[ "$got" == "$TONGSUO_COMMIT" ]] || { echo "Tongsuo commit mismatch: $got != $TONGSUO_COMMIT" >&2; exit 3; }
    DEP_OPTS=(--with-openssl="$TLS_DIR" --with-openssl-opt="enable-ntls no-shared no-tests" --build=gmssl)
  else
    TLS_DIR=$(fetch_dep openssl "$OPENSSL_URL" "$OPENSSL_SHA256")
    DEP_OPTS=(--with-openssl="$TLS_DIR" --with-openssl-opt="no-shared no-tests no-docs")
  fi
  DEP_OPTS+=(--with-pcre="$PCRE2_DIR" --with-pcre-jit --with-zlib="$ZLIB_DIR")
  # 静态 OpenSSL / PCRE2 只链入核心用到的目标文件：固定导出 Lua 附加包需要的函数（不影响任何行为）
  u=""
  while read -r sym; do
    [[ -z "$sym" || "$sym" == \#* ]] && continue
    u+=" -Wl,-u,$sym"
  done < "$HERE/lua/base-exports.txt"
  DEP_OPTS+=(--with-ld-opt="${u# }")
fi

ci_fail() { # <日志> <行数>：在 GitHub Actions 中把失败日志的末尾写成注解（原始日志不一定能下载）
  tail -"$2" "$1" >&2
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then tail -"$2" "$1" | sed 's/^/::error title=build-syapp::/'; fi
}

# 不编译 auth_basic：渲染器不使用它，去掉后二进制不再依赖 libcrypt，便于跨发行版运行
# auth_request：规则积木的外部鉴权（P10）
CONF=(\
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
  --with-http_auth_request_module \
  --without-http_auth_basic_module \
  "${DEP_OPTS[@]}")
./auto/configure "${CONF[@]}" > "$WORK/configure.log" 2>&1 || { ci_fail "$WORK/configure.log" 15; exit 5; }

make -j"$(nproc)" > "$WORK/make.log" 2>&1 || { ci_fail "$WORK/make.log" 20; exit 5; }
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
if [[ $STATIC -eq 1 ]]; then
  # 只允许依赖 glibc 自身的库
  extra=$(ldd "$O/sbin/syapp" | awk '{print $1}' | grep -vE '^(linux-vdso\.so|/lib.*/ld-linux|ld-linux|libc\.so|libm\.so|libdl\.so|libpthread\.so|librt\.so)' || true)
  [[ -z "$extra" ]] || { echo "static build still links: $extra" >&2; exit 4; }
  if [[ -n "${MAX_GLIBC:-}" ]]; then
    need=$(objdump -T "$O/sbin/syapp" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sed 's/GLIBC_//' | sort -uV | tail -1)
    [[ "$(printf '%s\n%s\n' "$need" "$MAX_GLIBC" | sort -V | tail -1)" == "$MAX_GLIBC" ]] || { echo "binary needs glibc $need > $MAX_GLIBC" >&2; exit 4; }
    echo "glibc requirement: $need (max allowed $MAX_GLIBC)"
  fi
fi

VARIANT=""
if [[ "$TLS_LIB" == "tongsuo" ]]; then
  VARIANT="gmssl-"
  "$O/sbin/syapp" -V 2>&1 | grep -q "enable-ntls" || { echo "GM build: enable-ntls missing from syapp -V" >&2; exit 4; }
  "$O/sbin/syapp" -v 2>&1 | grep -q "(gmssl)" || { echo "GM build: (gmssl) missing from syapp -v" >&2; exit 4; }
  if [[ -n "${KEEP_TLS_CLI:-}" ]]; then
    cli=$(find "$TLS_DIR" -path '*/apps/openssl' -type f -perm -u+x | head -1)
    [[ -n "$cli" ]] && cp "$cli" "$KEEP_TLS_CLI" && echo "Tongsuo CLI -> $KEEP_TLS_CLI"
  fi
fi

PKG="$OUT/syapp-$UPSTREAM_VERSION-$VARIANT$(uname -m).tar.gz"
tar -C "$ROOT" -czf "$PKG" opt
echo "BUILD OK: $PKG"

if [[ -n "${LUA_MODULES_OUT:-}" ]]; then
  : "${NDK_SRC:?}" "${LUA_MODULE_SRC:?}" "${LUAJIT_INC:?}" "${LUAJIT_LIB:?}"
  export LUAJIT_INC LUAJIT_LIB
  ./auto/configure "${CONF[@]}" --add-dynamic-module="$NDK_SRC" --add-dynamic-module="$LUA_MODULE_SRC" \
    > "$WORK/configure-lua.log" 2>&1 || { ci_fail "$WORK/configure-lua.log" 15; exit 6; }
  make -j"$(nproc)" modules > "$WORK/make-lua.log" 2>&1 || { ci_fail "$WORK/make-lua.log" 20; exit 6; }
  mkdir -p "$LUA_MODULES_OUT"
  for m in ndk_http_module ngx_http_lua_module; do
    install -m 0644 "objs/$m.so" "$LUA_MODULES_OUT/$m.so"
    strip --strip-unneeded "$LUA_MODULES_OUT/$m.so"
  done
  # 模块每个未定义的强符号（不带 glibc 版本号）都必须由基础包或先加载的 ngx_devel_kit 模块导出，否则 load_module 会失败
  { nm -D --defined-only "$O/sbin/syapp"; nm -D --defined-only "$LUA_MODULES_OUT/ndk_http_module.so"; } | awk '{print $NF}' | sort -u > "$WORK/exports"
  nm -D -u "$LUA_MODULES_OUT"/*.so | awk '$1 == "U" {print $2}' | grep -v '@' | sort -u > "$WORK/needs"
  missing=$(comm -23 "$WORK/needs" "$WORK/exports" | tr '\n' ' ')
  if [[ -n "${missing// }" ]]; then
    echo "Lua modules need symbols the base binary does not export: $missing" >&2
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=base exports::add to dataplane/lua/base-exports.txt: $missing"
    exit 7
  fi
  echo "LUA MODULES OK: $LUA_MODULES_OUT"
fi
