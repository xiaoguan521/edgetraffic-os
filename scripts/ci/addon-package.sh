#!/usr/bin/env bash
# 检查正式发布的国密升级包与对应 Lua 包的目录结构和版本绑定。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
PREFIX=${SYEDGE_PREFIX:-/opt/syedge}
[[ "$PREFIX" =~ ^(/[A-Za-z0-9._@+-]+)+$ ]] || { echo "SYEDGE_PREFIX must be a clean absolute path" >&2; exit 2; }
mkdir -p "$W/base$PREFIX/sbin" "$W/lua/addon/lualib" "$W/out"
printf 'test binary\n' > "$W/base$PREFIX/sbin/syedge"
tar -C "$W/base" -czf "$W/gm.tar.gz" "${PREFIX#/}"
printf '{"name":"syedge-lua","version":"1.30.6","syedge":"1.30.6"}\n' > "$W/lua/addon/VERSION"
printf '%s\n' "$PREFIX" > "$W/lua/addon/PREFIX"
printf 'syedge-lua 1.30.6\n' > "$W/lua/addon/NOTICE"
cp dataplane/lua/install.sh "$W/lua/install.sh"
cp dataplane/lua/uninstall.sh "$W/lua/uninstall.sh"
resolved=$(env -u SYEDGE_PREFIX -u SYEDGE_ROOT bash -c 'source "$1"; packaged_default_root' -- "$W/lua/install.sh")
[[ "$resolved" == "$PREFIX" ]] || { echo "Lua package lost its default prefix: $resolved" >&2; exit 1; }
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
[[ $(tar -xOzf "$lua" addon/PREFIX) == "$PREFIX" ]]
tar -tzf "$lua" | grep -qx 'install.sh'
if OUT="$W/out" scripts/release/package.sh lua-upgrade "$W/lua.tar.gz" 2.0.0-1 x86_64 >/dev/null 2>&1; then
  echo 'Lua package with mismatched base version was accepted' >&2; exit 1
fi
echo 'GM upgrade and matching Lua package: OK'
