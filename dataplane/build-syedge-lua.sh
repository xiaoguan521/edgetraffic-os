#!/usr/bin/env bash
# 构建 syedge-lua 附加包（P10，规格书 12.5）：同一个包同时适配标准版与国密版基础包。
#
# 用法：dataplane/build-syedge-lua.sh [-o 输出目录] [-v 包版本]
#   依赖按 dataplane/lua/DEPS 克隆并校验提交哈希；LuaJIT 编译为静态 PIC 库链入 lua 模块（模块只依赖 syedge 与 glibc）；
#   对标准版（OpenSSL）与国密版（铜锁）各执行一次 build-syedge.sh -S（与基础包完全相同的参数）并编译两个动态模块。
# 产物：
#   <输出目录>/syedge-lua-<包版本>-<架构>.tar.gz   附加包（install.sh / uninstall.sh + addon/）
#   <输出目录>/base/                              同一次构建产出的标准版与国密版基础包（CI 验收用）
# 包版本默认等于 syedge 上游版本；附加包只能装在同一包版本的 syedge 上（模块与二进制版本强绑定）。
set -euo pipefail
umask 022
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/UPSTREAM"
# shellcheck disable=SC1091
source "$HERE/lua/DEPS"
# 品牌（设计 0001 第 4 节）：与 build-syedge.sh 读取同一个 BRAND_ENV
# shellcheck disable=SC1090
source "${BRAND_ENV:-$HERE/../brand.env}"
B="${BRAND_ID:?brand.env: BRAND_ID required}"; P="${BRAND_PROC:-$B}"; DEFAULT_BRAND=syedge DEFAULT_PROC=SyEdge

OUT="$PWD/dist-lua"; PKG_VERSION="$UPSTREAM_VERSION"
while getopts "o:v:" opt; do
  case "$opt" in
    o) OUT="$OPTARG" ;;
    v) PKG_VERSION="$OPTARG" ;;
    *) exit 2 ;;
  esac
done
[[ "$PKG_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || { echo "bad package version $PKG_VERSION" >&2; exit 2; }
mkdir -p "$OUT/base"; OUT="$(cd "$OUT" && pwd)"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

pin() { # <名称> <git> <标签> <提交> -> 目录
  local dir="$WORK/src/$1"
  git -c advice.detachedHead=false clone -q --depth 1 --branch "$3" "$2" "$dir"
  local got; got="$(git -C "$dir" rev-parse HEAD)"
  [[ "$got" == "$4" ]] || { echo "$1: commit mismatch $got != $4" >&2; exit 3; }
  echo "$dir"
}
LUAJIT_SRC=$(pin luajit "$LUAJIT_GIT" "$LUAJIT_TAG" "$LUAJIT_COMMIT")
NDK_SRC=$(pin ndk "$NDK_GIT" "$NDK_TAG" "$NDK_COMMIT")
LUA_MODULE_SRC=$(pin lua-module "$LUA_MODULE_GIT" "$LUA_MODULE_TAG" "$LUA_MODULE_COMMIT")
RESTY_CORE_SRC=$(pin resty-core "$RESTY_CORE_GIT" "$RESTY_CORE_TAG" "$RESTY_CORE_COMMIT")
LRUCACHE_SRC=$(pin lrucache "$LRUCACHE_GIT" "$LRUCACHE_TAG" "$LRUCACHE_COMMIT")
RESTY_RSA_SRC=$(pin resty-rsa "$RESTY_RSA_GIT" "$RESTY_RSA_TAG" "$RESTY_RSA_COMMIT")

# LuaJIT：只要静态库，-fPIC 以便链入共享模块；模块构建直接使用源码目录的头文件
make -C "$LUAJIT_SRC" -j"$(nproc)" BUILDMODE=static CFLAGS="-fPIC -O2" > "$WORK/luajit.log" 2>&1 \
  || { tail -20 "$WORK/luajit.log" >&2; exit 5; }
mkdir -p "$WORK/luajit-lib"
cp "$LUAJIT_SRC/src/libluajit.a" "$WORK/luajit-lib/libluajit-5.1.a"
export LUAJIT_LIB="$WORK/luajit-lib" LUAJIT_INC="$LUAJIT_SRC/src"
export NDK_SRC LUA_MODULE_SRC

A="$WORK/pkg/addon"
mkdir -p "$A/lualib"
for tls in openssl tongsuo; do
  variant=standard; [[ "$tls" == tongsuo ]] && variant=gmssl
  LUA_MODULES_OUT="$A/$variant" "$HERE/build-syedge.sh" -S -t "$tls" -o "$OUT/base"
done

# Lua 库：lua-resty-core、lua-resty-lrucache、lua-resty-rsa 与 etos（审核过的规则库）
cp -r "$RESTY_CORE_SRC/lib/." "$A/lualib/"
cp -r "$LRUCACHE_SRC/lib/." "$A/lualib/"
mkdir -p "$A/lualib/resty"
cp "$RESTY_RSA_SRC/lib/resty/rsa.lua" "$A/lualib/resty/rsa.lua"
cp -r "$HERE/lua/lualib/etos" "$A/lualib/"
find "$A/lualib" -type f ! -name '*.lua' -delete
cat > "$A/VERSION" <<EOF
{"name":"syedge-lua","version":"$PKG_VERSION","syedge":"$UPSTREAM_VERSION","luajit":"$LUAJIT_TAG","ndk":"$NDK_TAG","lua_module":"$LUA_MODULE_TAG","resty_core":"$RESTY_CORE_TAG","lrucache":"$LRUCACHE_TAG","resty_rsa":"$RESTY_RSA_TAG"}
EOF
printf '%s\n' "${SYEDGE_PREFIX:-/opt/$B}" > "$A/PREFIX"
{
  echo "syedge-lua $PKG_VERSION"
  echo
  echo "Bundled third-party components (unmodified):"
  echo "  LuaJIT 2.1 ($LUAJIT_TAG, OpenResty branch)            MIT License"
  echo "  ngx_devel_kit ($NDK_TAG)                              BSD-3-Clause"
  echo "  OpenResty lua module ($LUA_MODULE_TAG)                BSD-2-Clause"
  echo "  lua-resty-core ($RESTY_CORE_TAG)                      BSD-2-Clause"
  echo "  lua-resty-lrucache ($LRUCACHE_TAG)                    BSD-2-Clause"
  echo "  lua-resty-rsa ($RESTY_RSA_TAG)                         MIT License"
  echo
  for f in "$LUAJIT_SRC/COPYRIGHT" "$NDK_SRC/LICENSE" "$RESTY_RSA_SRC/LICENSE"; do [[ -f "$f" ]] && { echo "---- $(basename "$(dirname "$f")")/$(basename "$f")"; cat "$f"; echo; }; done
} > "$A/NOTICE"
install -m 0755 "$HERE/lua/install.sh" "$WORK/pkg/install.sh"
install -m 0755 "$HERE/lua/uninstall.sh" "$WORK/pkg/uninstall.sh"

# 自检：两套模块齐全；模块之外（lualib、脚本）不含上游名
for v in standard gmssl; do
  for m in ndk_http_module ngx_http_lua_module; do [[ -s "$A/$v/$m.so" ]] || { echo "missing $v/$m.so" >&2; exit 4; }; done
done
if grep -rqi 'ngi''nx' "$A/lualib/etos" "$WORK/pkg/install.sh" "$WORK/pkg/uninstall.sh" "$A/VERSION"; then
  echo "upstream name in add-on scripts or etos library" >&2; exit 4
fi

# 定制品牌：附加包中的脚本、etos 库与说明按品牌改写（模块 .so 由按品牌构建的源码编译，不改动），并检查没有残留
if [[ "$B" != "$DEFAULT_BRAND" ]]; then
  grep -rlIZ -e "$DEFAULT_BRAND" -e "${DEFAULT_BRAND^^}" -e "$DEFAULT_PROC" "$WORK/pkg" | xargs -0 -r sed -i -e "s/$DEFAULT_PROC/$P/g" -e "s/$DEFAULT_BRAND/$B/g" -e "s/${DEFAULT_BRAND^^}/${B^^}/g"
  if grep -rlIi "$DEFAULT_BRAND" "$WORK/pkg"; then echo "default brand left in the add-on" >&2; exit 4; fi
fi

PKG="$OUT/$B-lua-$PKG_VERSION-$(uname -m).tar.gz"
tar -C "$WORK/pkg" --owner=0 --group=0 -czf "$PKG" install.sh uninstall.sh addon
echo "BUILD OK: $PKG"
