#!/usr/bin/env bash
# 把安装前缀下的 Lua 库目录编进 LuaJIT 的默认 package.path / package.cpath。
#   用法：dataplane/lua/patch-luajit-paths.sh <LuaJIT 源码目录> <安装根目录>
#
# 目的：运维写 Lua 时不用在每份主配置里都写 lua_package_path。没有设置 lua_package_path 的配置，
# require 会按下面的顺序先找本前缀下的库，再找 LuaJIT 原来的默认路径：
#   <R>/modules/lua/lualib   附加包自带库（resty.core、resty.lrucache、cjson 等）
#   <R>/modules/lua/extlib   随附的扩展库（只对独立模式与原生兼容配置可见）
#   <R>/lualib               运维自己的库（附加包升级不会覆盖）
#
# 边界：
#   - 只改写 LuaJIT 的默认值。配置里写了 lua_package_path / lua_package_cpath 时行为不变：
#     不带 ";;" 的写法是完全覆盖（托管渲染的 lua_package_path 就是这样，不会引用 extlib），带 ";;" 的写法会展开为这里的默认值。
#   - 路径在编译期写入，与基础包、wrapper 的编译期默认前缀（SYEDGE_PREFIX）一致；
#     自定义 system 根目录（/etc/syedge/prefix）的实例仍需在配置里显式写 lua_package_path，或使用 install.sh --standalone 生成的片段。
set -euo pipefail

SRC=${1:?用法：$0 <LuaJIT 源码目录> <安装根目录>}
ROOT=${2:?用法：$0 <LuaJIT 源码目录> <安装根目录>}
H="$SRC/src/luaconf.h"
[[ -f "$H" ]] || { echo "找不到 $H" >&2; exit 2; }
# 路径要进 C 字符串与 sed 替换，只接受保守字符
[[ "$ROOT" =~ ^/[A-Za-z0-9._/-]*[A-Za-z0-9._-]$ && "$ROOT" != *..* ]] || { echo "安装根目录不合法：$ROOT" >&2; exit 2; }

LJ="$ROOT/modules/lua/lualib" EX="$ROOT/modules/lua/extlib" OWN="$ROOT/lualib"
PATH_PREFIX="$LJ/?.lua;$LJ/?/init.lua;$EX/?.lua;$EX/?/init.lua;$OWN/?.lua;$OWN/?/init.lua;"
CPATH_PREFIX="$EX/?.so;$OWN/?.so;"

# 只改非 Windows 的两行；已经改过（重复执行）则不再改
if grep -q 'SYEDGE_DEFAULT_PATHS' "$H"; then echo "luaconf.h 已改写，跳过"; exit 0; fi
n_path=$(grep -c '^#define LUA_PATH_DEFAULT	"\./?\.lua" LUA_JPATH' "$H" || true)
n_cpath=$(grep -c '^#define LUA_CPATH_DEFAULT	"\./?\.so"' "$H" || true)
[[ "$n_path" == 1 && "$n_cpath" == 1 ]] || { echo "luaconf.h 的默认路径定义与预期不符（path=$n_path cpath=$n_cpath），LuaJIT 版本变了？" >&2; exit 3; }

tmp="$H.new"
{
  echo '/* SYEDGE_DEFAULT_PATHS: 由 dataplane/lua/patch-luajit-paths.sh 改写默认搜索路径 */'
  sed -e "s#^\(\#define LUA_PATH_DEFAULT	\)\"\\./?\\.lua\"#\\1\"$PATH_PREFIX\" \"./?.lua\"#" \
      -e "s#^\(\#define LUA_CPATH_DEFAULT	\)\"\\./?\\.so\"#\\1\"$CPATH_PREFIX\" \"./?.so\"#" "$H"
} > "$tmp"
grep -q "\"$PATH_PREFIX\" \"./?.lua\"" "$tmp" && grep -q "\"$CPATH_PREFIX\" \"./?.so\"" "$tmp" \
  || { rm -f "$tmp"; echo "改写 luaconf.h 失败" >&2; exit 4; }
mv "$tmp" "$H"
echo "luaconf.h 默认路径已指向 $ROOT"
