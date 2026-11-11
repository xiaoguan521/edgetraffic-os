#!/usr/bin/env bash
# 构建 syedge 数据面（规格书 4.3）。
#
# 用法：
#   dataplane/build-syedge.sh [-s <上游源码目录>] [-o <输出目录>] [-t openssl|tongsuo] [-S]
#
# 不指定 -s 时按 dataplane/UPSTREAM 从官方 GitHub 镜像拉取发布标签，并校验提交哈希。
# -t tongsuo：国密变体。铜锁按 dataplane/UPSTREAM 拉取发布标签并校验提交哈希，开启 NTLS 后静态链接（隐含 -S）；
#     configure 带 --build=gmssl，syedge -V 显示 "(gmssl)" 与 enable-ntls，控制台据此识别国密节点。
#     KEEP_TLS_CLI=<路径> 时把铜锁的命令行工具复制出来（CI 用它做国密握手测试，不进安装包）。
# LUA_MODULES_OUT=<目录>：基础包打包后，用完全相同的 configure 参数再编译 Lua 附加包的三个动态模块
#     （ndk_http_module、ngx_http_lua_module、ngx_http_lua_upstream_module；后者需要 LUA_UPSTREAM_SRC）
#     （ngx_devel_kit、lua 模块，需要 NDK_SRC / LUA_MODULE_SRC / LUAJIT_INC / LUAJIT_LIB），只输出 .so，
#     并核对模块需要的每个符号都由基础包或 ngx_devel_kit 导出（见 dataplane/lua/base-exports.txt）。由 build-syedge-lua.sh 调用。
# -S：OpenSSL、PCRE2、zlib 按 dataplane/DEPS 下载源码并静态链接，二进制只依赖 glibc。
#     在 glibc 2.17 的构建环境（manylinux2014）中执行，即可覆盖 CentOS 7、麒麟 V10、统信 UOS、openEuler 与较新的发行版。
#     DEPS_CACHE 指向已下载源码包的目录时优先使用缓存；MAX_GLIBC 设置后校验二进制不依赖更高版本的 glibc 符号。
# 产物：<输出目录>/<品牌>-<版本>-<架构>.tar.gz（国密变体为 <品牌>-<版本>-gmssl-<架构>.tar.gz），解压到 / 即得到编译期前缀目录树。
# 品牌：读取 BRAND_ENV（默认仓库根目录的 brand.env，设计 0001 第 4 节）。默认品牌使用已提交的改名 patch；
#     其他品牌由 dataplane/gen-rename-patch 从模板生成 patch（需要 Go），并检查产物中没有残留默认品牌名。
set -euo pipefail
umask 022
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/UPSTREAM"
# shellcheck disable=SC1090
source "${BRAND_ENV:-$HERE/../brand.env}"
B="${BRAND_ID:?brand.env: BRAND_ID required}"; P="${BRAND_PROC:-$B}"
[[ "$B" =~ ^[a-z][a-z0-9]{2,6}$ ]] || { echo "BRAND_ID must match ^[a-z][a-z0-9]{2,6}\$" >&2; exit 2; }
DEFAULT_BRAND=syedge DEFAULT_PROC=SyEdge
PREFIX="${SYEDGE_PREFIX:-/opt/$B}"
# shellcheck source=../scripts/validate-prefix.sh
source "$HERE/../scripts/validate-prefix.sh"
validate_system_prefix "$PREFIX" "/opt/$B" || { echo "SYEDGE_PREFIX must be a safe dedicated absolute directory" >&2; exit 2; }

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

# 只允许一个 patch；默认品牌直接使用它，其他品牌从模板生成（只替换字符串字面量与宏的值）
mapfile -t patches < <(ls "$HERE"/patches/*.patch)
[[ ${#patches[@]} -eq 1 ]] || { echo "expected exactly one patch, found ${#patches[@]}" >&2; exit 2; }
PATCH="${patches[0]}"
if [[ "$B" != "$DEFAULT_BRAND" || "$P" != "$DEFAULT_PROC" ]]; then
  PATCH="$WORK/rename-$B.patch"
  (cd "$HERE/.." && go run -mod=vendor ./dataplane/gen-rename-patch -id "$B" -proc "$P" -o "$PATCH")
fi
patch -p1 --forward --quiet < "$PATCH"
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
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then tail -"$2" "$1" | sed 's/^/::error title=build-syedge::/'; fi
}

# 不编译 auth_basic：渲染器不使用它，去掉后二进制不再依赖 libcrypt，便于跨发行版运行
# auth_request：规则积木的外部鉴权（P10）
CONF=(\
  --prefix="$PREFIX" \
  --sbin-path="$PREFIX/sbin/$B" \
  --conf-path="$PREFIX/conf/current/$B.conf" \
  --pid-path=/run/$B/$B.pid \
  --lock-path=/run/$B/$B.lock \
  --error-log-path=/var/log/$B/error.log \
  --http-log-path=/var/log/$B/access.log \
  --http-client-body-temp-path=/var/cache/$B/client_temp \
  --http-proxy-temp-path=/var/cache/$B/proxy_temp \
  --http-fastcgi-temp-path=/var/cache/$B/fastcgi_temp \
  --http-uwsgi-temp-path=/var/cache/$B/uwsgi_temp \
  --http-scgi-temp-path=/var/cache/$B/scgi_temp \
  --user=$B --group=$B \
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
O="$ROOT$PREFIX"
mkdir -p "$O/share/defaults" "$O/share/doc" "$O/conf/releases"
for f in mime.types fastcgi_params scgi_params uwsgi_params koi-utf koi-win win-utf; do
  [[ -f "$O/conf/current/$f" ]] && mv "$O/conf/current/$f" "$O/share/defaults/$f"
done
rm -rf "$O/conf/current"
# 上游参考文件中的旧名：变量已改名为 $<品牌>_version（原样保留会让 include 这些文件的独立模式配置报错）；
# 编码表注释里指向上游源码目录的说明删掉
for f in fastcgi_params fastcgi.conf; do
  [[ -f "$O/share/defaults/$f" ]] && sed -i "s#SERVER_SOFTWARE\\( *\\)ngi""nx/\\\$ngi""nx_version#SERVER_SOFTWARE\\1$P/\\\$${B}_version#" "$O/share/defaults/$f"
done
for f in koi-utf koi-win win-utf; do
  [[ -f "$O/share/defaults/$f" ]] && sed -i '/unicode2/d' "$O/share/defaults/$f"
done
cp "$HERE/packaging/html/index.html" "$HERE/packaging/html/50x.html" "$O/html/"
cp "$HERE/../docs/NOTICE" "$O/share/doc/NOTICE"
# 定制品牌：NOTICE 中的数据面名称随品牌改写（上游版权与 BSD 许可文本不变）
# 默认页面（html/）中的产品显示名同样随品牌改写
[[ "$B" != "$DEFAULT_BRAND" || "$P" != "$DEFAULT_PROC" ]] && sed -i -e "s/$DEFAULT_PROC/$P/g" -e "s/$DEFAULT_BRAND/$B/g" "$O/share/doc/NOTICE" "$O"/html/*.html
strip "$O/sbin/$B"

# 构建后自检
"$O/sbin/$B" -v 2>&1 | grep -q "^$P version: $P/$UPSTREAM_VERSION" || { echo "version check failed" >&2; exit 4; }
if strings "$O/sbin/$B" | grep -qi 'ngi''nx'; then
  echo "upstream name leaked into binary:" >&2
  strings "$O/sbin/$B" | grep -i 'ngi''nx' >&2
  exit 4
fi
# 定制品牌：产物中不得残留默认品牌名（二进制、文件内容与文件名）
if [[ "$B" != "$DEFAULT_BRAND" ]]; then
  if strings "$O/sbin/$B" | grep -qi "$DEFAULT_BRAND"; then
    echo "default brand leaked into binary:" >&2; strings "$O/sbin/$B" | grep -i "$DEFAULT_BRAND" >&2; exit 4
  fi
  dleaks=$(grep -rlai "$DEFAULT_BRAND" "$ROOT" || true)
  [[ -z "$dleaks" ]] || { echo "default brand in packaged files: $dleaks" >&2; exit 4; }
  if find "$ROOT" | grep -qi "$DEFAULT_BRAND"; then
    echo "default brand in file names:" >&2; find "$ROOT" | grep -i "$DEFAULT_BRAND" >&2; exit 4
  fi
fi
# 包内文件内容：只有 NOTICE（BSD 许可要求保留的上游版权与归因）可以出现上游名
leaks=$(grep -rlai 'ngi''nx' "$ROOT" | grep -v '/NOTICE$' | grep -v "/sbin/$B\$" || true)
if [[ -n "$leaks" ]]; then
  echo "upstream name in packaged files: $leaks" >&2; exit 4
fi
if find "$ROOT" | grep -qi 'ngi''nx'; then
  echo "upstream name leaked into file names:" >&2; find "$ROOT" | grep -i 'ngi''nx' >&2; exit 4
fi
if [[ $STATIC -eq 1 ]]; then
  # 只允许依赖 glibc 自身的库
  extra=$(ldd "$O/sbin/$B" | awk '{print $1}' | grep -vE '^(linux-vdso\.so|/lib.*/ld-linux|ld-linux|libc\.so|libm\.so|libdl\.so|libpthread\.so|librt\.so)' || true)
  [[ -z "$extra" ]] || { echo "static build still links: $extra" >&2; exit 4; }
  if [[ -n "${MAX_GLIBC:-}" ]]; then
    need=$(objdump -T "$O/sbin/$B" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sed 's/GLIBC_//' | sort -uV | tail -1)
    [[ "$(printf '%s\n%s\n' "$need" "$MAX_GLIBC" | sort -V | tail -1)" == "$MAX_GLIBC" ]] || { echo "binary needs glibc $need > $MAX_GLIBC" >&2; exit 4; }
    echo "glibc requirement: $need (max allowed $MAX_GLIBC)"
  fi
fi

VARIANT=""
if [[ "$TLS_LIB" == "tongsuo" ]]; then
  VARIANT="gmssl-"
  "$O/sbin/$B" -V 2>&1 | grep -q "enable-ntls" || { echo "GM build: enable-ntls missing from syedge -V" >&2; exit 4; }
  "$O/sbin/$B" -v 2>&1 | grep -q "(gmssl)" || { echo "GM build: (gmssl) missing from syedge -v" >&2; exit 4; }
  if [[ -n "${KEEP_TLS_CLI:-}" ]]; then
    cli=$(find "$TLS_DIR" -path '*/apps/openssl' -type f -perm -u+x | head -1)
    [[ -n "$cli" ]] && cp "$cli" "$KEEP_TLS_CLI" && echo "Tongsuo CLI -> $KEEP_TLS_CLI"
  fi
fi

PKG="$OUT/$B-$UPSTREAM_VERSION-$VARIANT$(uname -m).tar.gz"
tar -C "$ROOT" --owner=0 --group=0 -czf "$PKG" "${PREFIX#/}"
echo "BUILD OK: $PKG"

if [[ -n "${LUA_MODULES_OUT:-}" ]]; then
  : "${NDK_SRC:?}" "${LUA_MODULE_SRC:?}" "${LUA_UPSTREAM_SRC:?}" "${LUAJIT_INC:?}" "${LUAJIT_LIB:?}"
  export LUAJIT_INC LUAJIT_LIB
  # lua-upstream（ngx.upstream）是第三个动态模块：与 lua 模块同一次 configure，参数完全相同
  ./auto/configure "${CONF[@]}" --add-dynamic-module="$NDK_SRC" --add-dynamic-module="$LUA_MODULE_SRC" \
    --add-dynamic-module="$LUA_UPSTREAM_SRC" \
    > "$WORK/configure-lua.log" 2>&1 || { ci_fail "$WORK/configure-lua.log" 15; exit 6; }
  make -j"$(nproc)" modules > "$WORK/make-lua.log" 2>&1 || { ci_fail "$WORK/make-lua.log" 20; exit 6; }
  mkdir -p "$LUA_MODULES_OUT"
  for m in ndk_http_module ngx_http_lua_module ngx_http_lua_upstream_module; do
    install -m 0644 "objs/$m.so" "$LUA_MODULES_OUT/$m.so"
    strip --strip-unneeded "$LUA_MODULES_OUT/$m.so"
  done
  # 模块每个未定义的强符号（不带 glibc 版本号）都必须由基础包或先加载的 ngx_devel_kit 模块导出，否则 load_module 会失败
  { nm -D --defined-only "$O/sbin/$B"; nm -D --defined-only "$LUA_MODULES_OUT/ndk_http_module.so"; } | awk '{print $NF}' | sort -u > "$WORK/exports"
  # Include symbols exported by each module's NEEDED libraries.  Some Kylin V10
  # binutils omit glibc version suffixes, which otherwise makes normal libc/
  # libgcc dependencies look like missing base exports.
  nm -D -u "$LUA_MODULES_OUT/ndk_http_module.so" "$LUA_MODULES_OUT/ngx_http_lua_module.so" | awk '$1 == "U" {print $2}' | sed 's/@.*//' | sort -u > "$WORK/needs"
  : > "$WORK/runtime-exports"
  if command -v ldd >/dev/null 2>&1; then
    for module in "$LUA_MODULES_OUT/ndk_http_module.so" "$LUA_MODULES_OUT/ngx_http_lua_module.so"; do
      ldd "$module" 2>/dev/null \
        | awk '$1 ~ /^\// {print $1} $3 == "=>" && $4 ~ /^\// {print $4}'
    done | sort -u \
      | while IFS= read -r lib; do
          [[ -f "$lib" ]] || continue
          nm -D --defined-only "$lib" 2>/dev/null | awk '{print $NF}' | sed 's/@.*//'
        done | sort -u > "$WORK/runtime-exports"
  fi
  # A module may intentionally leave libc/libm/libdl/libpthread/librt/libgcc
  # unresolved (the main process supplies them) without recording a DT_NEEDED
  # entry.  Add the standard runtime libraries explicitly for that toolchain
  # layout; the globs are harmless when a platform does not have that library.
  {
    for lib in /lib*/libc.so.6 /usr/lib*/libc.so.6 \
      /lib*/libm.so.6 /usr/lib*/libm.so.6 \
      /lib*/libdl.so.2 /usr/lib*/libdl.so.2 \
      /lib*/libpthread.so.0 /usr/lib*/libpthread.so.0 \
      /lib*/librt.so.1 /usr/lib*/librt.so.1 \
      /lib*/libgcc_s.so.1 /usr/lib*/libgcc_s.so.1; do
      [[ -f "$lib" ]] && printf '%s\n' "$lib"
    done
    if command -v ldconfig >/dev/null 2>&1; then
      ldconfig -p 2>/dev/null \
        | awk '$1 ~ /^(libc|libm|libdl|libpthread|librt|libgcc_s)\.so/ && $NF ~ /^\// {print $NF}'
    fi
  } | sort -u | while IFS= read -r lib; do
    nm -D --defined-only "$lib" 2>/dev/null | awk '{print $NF}' | sed 's/@.*//'
  done | sort -u >> "$WORK/runtime-exports"
  cat "$WORK/exports" "$WORK/runtime-exports" | sort -u > "$WORK/resolvable"
  missing=$(comm -23 "$WORK/needs" "$WORK/resolvable" | tr '\n' ' ')
  if [[ -n "${missing// }" ]]; then
    echo "Lua modules need symbols the base binary does not export: $missing" >&2
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=base exports::add to dataplane/lua/base-exports.txt: $missing"
    exit 7
  fi
  # lua-upstream 模块（D1 第二阶段）：它的 lua_* API 与 ngx_http_lua_add_package_preload 由 lua 模块导出（lua 模块以 RTLD_GLOBAL 加载），
  # 所以它的未定义符号可由基础包、ngx_devel_kit、运行时库或 lua 模块提供。
  nm -D -u "$LUA_MODULES_OUT/ngx_http_lua_upstream_module.so" | awk '$1 == "U" {print $2}' | sed 's/@.*//' | sort -u > "$WORK/needs-upstream"
  nm -D --defined-only "$LUA_MODULES_OUT/ngx_http_lua_module.so" | awk '{print $NF}' | sed 's/@.*//' | sort -u > "$WORK/lua-defined"
  cat "$WORK/resolvable" "$WORK/lua-defined" | sort -u > "$WORK/resolvable-upstream"
  missing=$(comm -23 "$WORK/needs-upstream" "$WORK/resolvable-upstream" | tr '\n' ' ')
  if [[ -n "${missing// }" ]]; then
    echo "lua-upstream module needs symbols nothing provides: $missing" >&2
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=lua-upstream::unresolved: $missing"
    exit 7
  fi
  # 附加库（extlib / lualib 中经 FFI 调用的 C 函数，见 base-exports.txt）：列出的每个符号都必须由基础包动态导出。
  # 库是运行时经 dlsym 解析的，load_module 与 syedge -t 都不会触发，所以这里直接核对导出表（标准版与铜锁各执行一次）。
  nm -D --defined-only "$O/sbin/$B" | awk '{print $NF}' | sed 's/@.*//' | sort -u > "$WORK/base-defined"
  # 只核对 “# 附加库 extlib” 之后的条目：之前的条目供 Lua 模块链接用，其中个别在某些 TLS 库版本里是宏，由上面的核对负责。
  awk '/^# 附加库 extlib/{on=1} on && !/^#/ && NF' "$HERE/lua/base-exports.txt" | sort -u > "$WORK/base-wanted"
  [[ -s "$WORK/base-wanted" ]] || { echo "base-exports.txt: no extlib section found" >&2; exit 7; }
  not_exported=$(comm -23 "$WORK/base-wanted" "$WORK/base-defined" | tr '\n' ' ')
  if [[ -n "${not_exported// }" ]]; then
    echo "base binary does not export symbols listed in base-exports.txt: $not_exported" >&2
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=base exports::not exported by the base binary: $not_exported"
    exit 7
  fi
  echo "LUA MODULES OK: $LUA_MODULES_OUT"
fi
