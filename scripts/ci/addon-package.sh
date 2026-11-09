#!/usr/bin/env bash
# 检查正式发布的国密升级包与对应 Lua 包的目录结构和版本绑定。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
mkdir -p "$W/base/opt/syedge/sbin" "$W/lua/addon/lualib" "$W/out"
printf 'test binary\n' > "$W/base/opt/syedge/sbin/syedge"
tar -C "$W/base" -czf "$W/gm.tar.gz" opt
printf '{"name":"syedge-lua","version":"1.30.6","syedge":"1.30.6"}\n' > "$W/lua/addon/VERSION"
printf 'syedge-lua 1.30.6\n' > "$W/lua/addon/NOTICE"
touch "$W/lua/install.sh" "$W/lua/uninstall.sh"
tar -C "$W/lua" -czf "$W/lua.tar.gz" install.sh uninstall.sh addon

OUT="$W/out" SYEDGE_UPGRADE_VERSION=1.30.6-1 scripts/release/package.sh syedge-upgrade amd64 "$W/gm.tar.gz" gmssl
OUT="$W/out" scripts/release/package.sh lua-upgrade "$W/lua.tar.gz" 1.30.6-1 x86_64
gm="$W/out/syedge-1.30.6-1-gmssl-amd64.tar.gz"
lua="$W/out/syedge-lua-1.30.6-1-x86_64.tar.gz"
[[ -f "$gm" && -f "$lua" ]]
[[ $(tar -xOzf "$gm" VERSION) == 1.30.6-1 ]]
[[ $(tar -xOzf "$gm" sbin/syedge) == 'test binary' ]]
! tar -tzf "$gm" | grep -q '^opt/'
tar -xOzf "$lua" addon/VERSION | grep -q '"version":"1.30.6-1"'
tar -xOzf "$lua" addon/VERSION | grep -q '"syedge":"1.30.6"'
tar -tzf "$lua" | grep -qx 'install.sh'
if OUT="$W/out" scripts/release/package.sh lua-upgrade "$W/lua.tar.gz" 2.0.0-1 x86_64 >/dev/null 2>&1; then
  echo 'Lua package with mismatched base version was accepted' >&2; exit 1
fi
echo 'GM upgrade and matching Lua package: OK'
