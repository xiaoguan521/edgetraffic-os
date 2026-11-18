#!/usr/bin/env bash
# 构建 syedge-lua 附加包（P10，规格书 12.5）：同一个包同时适配标准版与国密版基础包。
#
# 用法：dataplane/build-syedge-lua.sh [-o 输出目录] [-v 包版本]
#   依赖按 dataplane/lua/DEPS 克隆并校验提交哈希；LuaJIT 编译为静态 PIC 库链入 lua 模块（模块只依赖 syedge 与 glibc）；
#   对标准版（OpenSSL）与国密版（铜锁）各执行一次 build-syedge.sh -S（与基础包完全相同的参数）并编译三个动态模块
#   （ndk_http_module、ngx_http_lua_module、ngx_http_lua_upstream_module）。
#   附加库 extlib（安装到 <M>/lua/extlib/，只有独立模式与原生兼容配置包使用）：
#     第一阶段：纯 Lua 库（只复制 lib/ 下的 .lua）；
#     第二阶段：编译库 cjson.so、redis/parser.so、rds/parser.so、librestysignal.so（直接用 cc 编译，只依赖 LuaJIT 头文件），
#     以及 resty.signal / resty.upstream.healthcheck 的 Lua 文件。编译库与 TLS 库无关，两个变体共用。
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
LUA_UPSTREAM_SRC=$(pin lua-upstream "$LUA_UPSTREAM_GIT" "$LUA_UPSTREAM_TAG" "$LUA_UPSTREAM_COMMIT")
RESTY_CORE_SRC=$(pin resty-core "$RESTY_CORE_GIT" "$RESTY_CORE_TAG" "$RESTY_CORE_COMMIT")
LRUCACHE_SRC=$(pin lrucache "$LRUCACHE_GIT" "$LRUCACHE_TAG" "$LRUCACHE_COMMIT")
RESTY_RSA_SRC=$(pin resty-rsa "$RESTY_RSA_GIT" "$RESTY_RSA_TAG" "$RESTY_RSA_COMMIT")
grep -q '^The MIT License (MIT)' "$RESTY_RSA_SRC/LICENSE" \
  && grep -q 'Permission is hereby granted, free of charge' "$RESTY_RSA_SRC/LICENSE" \
  || { echo "lua-resty-rsa: license is not MIT" >&2; exit 4; }

# 默认搜索路径指向安装前缀（与下面写入 PREFIX 文件的值相同）：运维写 Lua 时不用在每份主配置里都写 lua_package_path。
# 配置里写了 lua_package_path 时行为不变（不带 ;; 是覆盖，带 ;; 是追加）。见 dataplane/lua/patch-luajit-paths.sh。
bash "$HERE/lua/patch-luajit-paths.sh" "$LUAJIT_SRC" "${SYEDGE_PREFIX:-/opt/$B}" || exit 6
# LuaJIT：只要静态库，-fPIC 以便链入共享模块；模块构建直接使用源码目录的头文件
make -C "$LUAJIT_SRC" -j"$(nproc)" BUILDMODE=static CFLAGS="-fPIC -O2" > "$WORK/luajit.log" 2>&1 \
  || { tail -20 "$WORK/luajit.log" >&2; exit 5; }
mkdir -p "$WORK/luajit-lib"
cp "$LUAJIT_SRC/src/libluajit.a" "$WORK/luajit-lib/libluajit-5.1.a"
export LUAJIT_LIB="$WORK/luajit-lib" LUAJIT_INC="$LUAJIT_SRC/src"
export NDK_SRC LUA_MODULE_SRC LUA_UPSTREAM_SRC

A="$WORK/pkg/addon"
mkdir -p "$A/lualib"
for tls in openssl tongsuo; do
  variant=standard; [[ "$tls" == tongsuo ]] && variant=gmssl
  LUA_MODULES_OUT="$A/$variant" "$HERE/build-syedge.sh" -S -t "$tls" -o "$OUT/base"
done

# Lua 库：lua-resty-core、lua-resty-lrucache 与 etos（审核过的规则库）
cp -r "$RESTY_CORE_SRC/lib/." "$A/lualib/"
cp -r "$LRUCACHE_SRC/lib/." "$A/lualib/"
cp -r "$HERE/lua/lualib/etos" "$A/lualib/"
find "$A/lualib" -type f ! -name '*.lua' -delete

# 附加库 extlib（D1）：安装到 <M>/lua/extlib/。控制台托管的 lua_package_path 与 cpath 不引用它（internal/render 不生成）。
# 许可在克隆后校验（README 声明 BSD；cjson 的 LICENSE 为 MIT）；模块名与 lualib 冲突即失败。
EXT_LIST="$WORK/ext-list"; EXT_NOTICE="$WORK/ext-notice"; : > "$EXT_LIST"; : > "$EXT_NOTICE"
EXT_TAGS=""
mkdir -p "$A/extlib"
readme_of() { find "$1" -maxdepth 1 -iname 'README*' | sort | head -1; }
bsd_check() { # <名称> <源码目录>：README 中的许可声明（licensed / licenced 两种拼写）
  local rd; rd=$(readme_of "$2")
  if [[ -z "$rd" ]] || ! grep -qiE 'licen[cs]ed under the BSD license' "$rd"; then
    echo "$1: license is not BSD ($rd)" >&2; exit 4
  fi
}
# 许可小节（NOTICE 用）：LICENSE 整文件原样收入；README 取 “Copyright and License / Copyright & License” 小节
notice_text() { # <许可文件或 README>
  case "$(basename "$1")" in
    LICENSE*) cat "$1" ;;
    *) awk '/^Copyright (and|&) License/{p=1; next} /^\[Back to TOC\]/{p=0} p' "$1" | grep -v '^=\+$' || true ;;
  esac
}
register() { # <名称> <标签> <许可> <许可文件>：版本清单、NOTICE 段与 VERSION 字段
  printf '  %-30s %-8s %s\n' "$1" "$2" "$3" >> "$EXT_LIST"
  { echo "---- $1 $2"; notice_text "$4"; echo; } >> "$EXT_NOTICE"
  EXT_TAGS+="$1 $2, "
}
ext_copy() { # <名称> <源码目录>：复制 lib/ 下的 .lua，拒绝模块名冲突
  local src="$2" rel f
  while IFS= read -r -d '' f; do
    rel="${f#"$src/lib/"}"
    [[ ! -e "$A/extlib/$rel" && ! -e "$A/lualib/$rel" ]] || { echo "$1: module $rel is already provided" >&2; exit 4; }
    mkdir -p "$A/extlib/$(dirname "$rel")"
    cp "$f" "$A/extlib/$rel"
  done < <(find "$src/lib" -type f -name '*.lua' -print0)
}
ext_add() { # <名称> <标签> <源码目录>：复制 lib/ 下的 .lua（BSD）并登记
  local src="$3"
  bsd_check "$1" "$src"
  ext_copy "$1" "$src"
  register "$1" "$2" BSD-2-Clause "$(readme_of "$src")"
}
ext_lib() { # <名称> <git> <标签> <提交>（纯 Lua 库，第一阶段）
  local src
  src=$(pin "$1" "$2" "$3" "$4")
  ext_add "$1" "$3" "$src"
}
ext_lib lua-resty-redis "$RESTY_REDIS_GIT" "$RESTY_REDIS_TAG" "$RESTY_REDIS_COMMIT"
ext_lib lua-resty-mysql "$RESTY_MYSQL_GIT" "$RESTY_MYSQL_TAG" "$RESTY_MYSQL_COMMIT"
ext_lib lua-resty-memcached "$RESTY_MEMCACHED_GIT" "$RESTY_MEMCACHED_TAG" "$RESTY_MEMCACHED_COMMIT"
ext_lib lua-resty-dns "$RESTY_DNS_GIT" "$RESTY_DNS_TAG" "$RESTY_DNS_COMMIT"
ext_lib lua-resty-string "$RESTY_STRING_GIT" "$RESTY_STRING_TAG" "$RESTY_STRING_COMMIT"
ext_lib lua-resty-upload "$RESTY_UPLOAD_GIT" "$RESTY_UPLOAD_TAG" "$RESTY_UPLOAD_COMMIT"
ext_lib lua-resty-websocket "$RESTY_WEBSOCKET_GIT" "$RESTY_WEBSOCKET_TAG" "$RESTY_WEBSOCKET_COMMIT"
ext_lib lua-resty-lock "$RESTY_LOCK_GIT" "$RESTY_LOCK_TAG" "$RESTY_LOCK_COMMIT"
ext_lib lua-resty-limit-traffic "$RESTY_LIMIT_TRAFFIC_GIT" "$RESTY_LIMIT_TRAFFIC_TAG" "$RESTY_LIMIT_TRAFFIC_COMMIT"
ext_lib lua-tablepool "$TABLEPOOL_GIT" "$TABLEPOOL_TAG" "$TABLEPOOL_COMMIT"
# lua-resty-rsa 来自 spacewander，许可已在构建开始时核验。
ext_copy lua-resty-rsa "$RESTY_RSA_SRC"
register lua-resty-rsa "$RESTY_RSA_TAG" MIT "$RESTY_RSA_SRC/LICENSE"

# 编译库（D1 第二阶段）：直接用 cc 编译，只依赖 LuaJIT 头文件（$LUAJIT_INC），不链接 libluajit。
# Lua API 符号在加载时由 ngx_http_lua_module.so 解析（nginx 以 RTLD_GLOBAL 加载它，LuaJIT 静态链入其中）。
# 与 TLS 库无关：两个变体共用同一份 .so，下面对两个变体分别核对符号。
CC_CFLAGS=(-O2 -fPIC -shared -I"$LUAJIT_INC")
mkdir -p "$A/extlib/redis" "$A/extlib/rds"

CJSON_SRC=$(pin lua-cjson "$CJSON_GIT" "$CJSON_TAG" "$CJSON_COMMIT")
grep -q 'Permission is hereby granted, free of charge' "$CJSON_SRC/LICENSE" || { echo "lua-cjson: license is not MIT" >&2; exit 4; }
# cjson 的默认配置：libc 数值转换（fpconv），不启用 dtoa / 多线程宏
cc "${CC_CFLAGS[@]}" -DNDEBUG -o "$A/extlib/cjson.so" \
  "$CJSON_SRC/lua_cjson.c" "$CJSON_SRC/strbuf.c" "$CJSON_SRC/fpconv.c" > "$WORK/cjson.log" 2>&1 \
  || { tail -20 "$WORK/cjson.log" >&2; exit 5; }
register lua-cjson "$CJSON_TAG" MIT "$CJSON_SRC/LICENSE"

REDIS_PARSER_SRC=$(pin lua-redis-parser "$REDIS_PARSER_GIT" "$REDIS_PARSER_TAG" "$REDIS_PARSER_COMMIT")
bsd_check lua-redis-parser "$REDIS_PARSER_SRC"
cc "${CC_CFLAGS[@]}" -o "$A/extlib/redis/parser.so" "$REDIS_PARSER_SRC/redis-parser.c" > "$WORK/redis.log" 2>&1 \
  || { tail -20 "$WORK/redis.log" >&2; exit 5; }
register lua-redis-parser "$REDIS_PARSER_TAG" BSD-2-Clause "$(readme_of "$REDIS_PARSER_SRC")"

RDS_PARSER_SRC=$(pin lua-rds-parser "$RDS_PARSER_GIT" "$RDS_PARSER_TAG" "$RDS_PARSER_COMMIT")
bsd_check lua-rds-parser "$RDS_PARSER_SRC"
cc "${CC_CFLAGS[@]}" -o "$A/extlib/rds/parser.so" "$RDS_PARSER_SRC/src/rds_parser.c" > "$WORK/rds.log" 2>&1 \
  || { tail -20 "$WORK/rds.log" >&2; exit 5; }
register lua-rds-parser "$RDS_PARSER_TAG" BSD-2-Clause "$(readme_of "$RDS_PARSER_SRC")"

# resty.signal：Lua 文件 + librestysignal.so（纯 C，只依赖 libc；resty.signal 按 package.cpath 查找该 .so）
RESTY_SIGNAL_SRC=$(pin lua-resty-signal "$RESTY_SIGNAL_GIT" "$RESTY_SIGNAL_TAG" "$RESTY_SIGNAL_COMMIT")
ext_add lua-resty-signal "$RESTY_SIGNAL_TAG" "$RESTY_SIGNAL_SRC"
cc -O2 -fPIC -shared -o "$A/extlib/librestysignal.so" "$RESTY_SIGNAL_SRC/resty_signal.c" > "$WORK/signal.log" 2>&1 \
  || { tail -20 "$WORK/signal.log" >&2; exit 5; }

# resty.upstream.healthcheck（依赖 ngx.upstream）：纯 Lua。
# lua-resty-shell 不随附：它依赖的 ngx.pipe 只在核心带有 OpenResty 的 socket CLOEXEC 补丁时才编译进 lua 模块，本核心不打该补丁。
RESTY_UPSTREAM_HC_SRC=$(pin lua-resty-upstream-healthcheck "$RESTY_UPSTREAM_HC_GIT" "$RESTY_UPSTREAM_HC_TAG" "$RESTY_UPSTREAM_HC_COMMIT")
ext_add lua-resty-upstream-healthcheck "$RESTY_UPSTREAM_HC_TAG" "$RESTY_UPSTREAM_HC_SRC"

# 编译库的符号核对：每个未定义符号都必须由动态依赖库（libc 等，ldd 解析到的）或两个变体的 lua 模块导出。
# 用 nm 读取，失败时列出缺少的符号并终止构建。
native_check() { # <so>
  local so="$1" v missing
  nm -D -u "$so" | awk '$1 == "U" {print $2}' | sed 's/@.*//' | sort -u > "$WORK/nc-needs"
  # 动态加载器本身也导出符号（aarch64 的 __stack_chk_guard 在 ld-linux-aarch64.so.1 中）；ldd 把它列为不带 “=>” 的一行，
  # 个别环境不列出，所以再按固定位置补上
  { ldd "$so" | awk '$1 ~ /^\// {print $1} $2 == "=>" && $3 ~ /^\// {print $3}'
    for l in /lib/ld-linux*.so.* /lib64/ld-linux*.so.* /lib/*/ld-linux*.so.*; do if [[ -f "$l" ]]; then printf '%s\n' "$l"; fi; done
  } | sort -u | while IFS= read -r l; do
    nm -D --defined-only "$l" 2>/dev/null | awk '{print $NF}' | sed 's/@.*//'
  done | sort -u > "$WORK/nc-runtime"
  for v in standard gmssl; do
    nm -D --defined-only "$A/$v/ngx_http_lua_module.so" | awk '{print $NF}' | sed 's/@.*//' | sort -u > "$WORK/nc-lua"
    missing=$(comm -23 "$WORK/nc-needs" <(cat "$WORK/nc-runtime" "$WORK/nc-lua" | sort -u) | tr '\n' ' ')
    if [[ -n "${missing// }" ]]; then
      echo "$(basename "$so") ($v) needs symbols nothing provides: $missing" >&2
      [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=extlib::$(basename "$so") ($v): $missing"
      exit 7
    fi
  done
}
for so in "$A/extlib/cjson.so" "$A/extlib/redis/parser.so" "$A/extlib/rds/parser.so" "$A/extlib/librestysignal.so"; do
  strip --strip-unneeded "$so"
  chmod 0644 "$so"
  native_check "$so"
done
EXT_TAGS=${EXT_TAGS%, }
cat > "$A/VERSION" <<EOF
{"name":"syedge-lua","version":"$PKG_VERSION","syedge":"$UPSTREAM_VERSION","luajit":"$LUAJIT_TAG","ndk":"$NDK_TAG","lua_module":"$LUA_MODULE_TAG","lua_upstream":"$LUA_UPSTREAM_TAG","resty_core":"$RESTY_CORE_TAG","lrucache":"$LRUCACHE_TAG","extlib":"$EXT_TAGS"}
EOF
printf '%s\n' "${SYEDGE_PREFIX:-/opt/$B}" > "$A/PREFIX"
UPSTREAM_NOTICE="$WORK/notice-upstream"
{ echo "---- lua-upstream-nginx-module $LUA_UPSTREAM_TAG"; notice_text "$(readme_of "$LUA_UPSTREAM_SRC")"; echo; } > "$UPSTREAM_NOTICE"
bsd_check lua-upstream-nginx-module "$LUA_UPSTREAM_SRC"
{
  echo "syedge-lua $PKG_VERSION"
  echo
  echo "Bundled third-party components (unmodified):"
  echo "  LuaJIT 2.1 ($LUAJIT_TAG, OpenResty branch)            MIT License"
  echo "  ngx_devel_kit ($NDK_TAG)                              BSD-3-Clause"
  echo "  OpenResty lua module ($LUA_MODULE_TAG)                BSD-2-Clause"
  echo "  lua-upstream-nginx-module ($LUA_UPSTREAM_TAG)         BSD-2-Clause"
  echo "  lua-resty-core ($RESTY_CORE_TAG)                      BSD-2-Clause"
  echo "  lua-resty-lrucache ($LRUCACHE_TAG)                    BSD-2-Clause"
  echo
  echo "Bundled Lua libraries (unmodified; installed under <modules>/lua/extlib/; C libraries are compiled from the unmodified sources):"
  cat "$EXT_LIST"
  echo
  for f in "$LUAJIT_SRC/COPYRIGHT" "$NDK_SRC/LICENSE"; do [[ -f "$f" ]] && { echo "---- $(basename "$(dirname "$f")")/$(basename "$f")"; cat "$f"; echo; }; done
  cat "$UPSTREAM_NOTICE"
  cat "$EXT_NOTICE"
} > "$A/NOTICE"
install -m 0755 "$HERE/lua/install.sh" "$WORK/pkg/install.sh"
install -m 0755 "$HERE/lua/uninstall.sh" "$WORK/pkg/uninstall.sh"

# 自检：两套模块齐全；模块之外（lualib、脚本）不含上游名
for v in standard gmssl; do
  for m in ndk_http_module ngx_http_lua_module ngx_http_lua_upstream_module; do [[ -s "$A/$v/$m.so" ]] || { echo "missing $v/$m.so" >&2; exit 4; }; done
done
for f in cjson.so redis/parser.so rds/parser.so librestysignal.so resty/rsa.lua; do [[ -s "$A/extlib/$f" ]] || { echo "missing extlib/$f" >&2; exit 4; }; done
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
