#!/usr/bin/env bash
# syedge-lua 附加包安装（P10）：在节点上以 root 执行  bash install.sh
# - 附加包必须与运行中的 syedge 版本一致（动态模块与二进制版本强绑定）；
# - 自动选择标准版或国密版对应的模块；
# - 安装到 /opt/syedge/modules/lua-<包版本>/，再原子切换软链 /opt/syedge/modules/lua；
# - 用真实 syedge -t 自检，失败则恢复原状。不重启、不停止 syedge，也不改动任何已发布的配置。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${SYEDGE_ROOT:-/opt/syedge}"
SY="$ROOT/sbin/syedge"
die() { echo "syedge-lua: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 || -n "${SYEDGE_ROOT:-}" ]] || die "请以 root 执行"
[[ -x "$SY" ]] || die "没有找到 $SY：请先安装 syedge 基础包"
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$HERE/addon/VERSION"; }
pkg=$(field version); want=$(field syedge)
[[ "$pkg" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || die "附加包版本无效"
running=$("$SY" -v 2>&1 | sed -n 's|.*syedge/\([0-9][0-9.]*\).*|\1|p')
[[ "$running" == "$want" ]] || die "附加包适用于 syedge $want，当前节点运行 syedge ${running:-未知}；请安装对应版本的附加包"
base=standard
"$SY" -V 2>&1 | grep -q "enable-ntls" && base=gmssl

M="$ROOT/modules"
mkdir -p "$M"
# 模块目录本身交给数据面用户：wrapper（以 syedge 运行，运行时不使用 root）热升级时在这里安装新版本并切换软链；
# 已安装的模块文件仍由 root 拥有
[[ -n "${SYEDGE_ROOT:-}" ]] || { id syedge >/dev/null 2>&1 && chown syedge:syedge "$M"; }
dest="$M/lua-$pkg"; tmp="$M/.lua-$pkg.tmp"
rm -rf "$tmp"; mkdir -p "$tmp"
install -m 0644 "$HERE/addon/$base/ndk_http_module.so" "$HERE/addon/$base/ngx_http_lua_module.so" "$tmp/"
cp -r "$HERE/addon/lualib" "$tmp/lualib"
install -m 0644 "$HERE/addon/VERSION" "$HERE/addon/NOTICE" "$tmp/"
echo "$base" > "$tmp/BASE"
chmod -R u=rwX,go=rX "$tmp"
[[ -n "${SYEDGE_ROOT:-}" ]] || chown -R root:root "$tmp"

prev=""
[[ -L "$M/lua" ]] && prev=$(readlink "$M/lua")
rm -rf "$dest.old"; [[ -e "$dest" ]] && mv "$dest" "$dest.old"
mv "$tmp" "$dest"
ln -sfn "lua-$pkg" "$M/lua.new" && mv -T "$M/lua.new" "$M/lua"

restore() {
  rm -rf "$dest"; [[ -e "$dest.old" ]] && mv "$dest.old" "$dest"
  if [[ -n "$prev" ]]; then ln -sfn "$prev" "$M/lua.new" && mv -T "$M/lua.new" "$M/lua"; else rm -f "$M/lua"; fi
}

# 自检：加载模块与附加包的 Lua 库
T=$(mktemp -d)
cat > "$T/t.conf" <<CONF
pid        $T/t.pid;
error_log  stderr  warn;
load_module  $M/lua/ndk_http_module.so;
load_module  $M/lua/ngx_http_lua_module.so;
events { worker_connections 16; }
http {
    access_log  off;
    lua_package_path   "$M/lua/lualib/?.lua";
    lua_package_cpath  "$M/lua/lualib/?.disabled";
    init_by_lua_block { require("etos.json"); require("resty.lrucache") }
}
CONF
if ! out=$("$SY" -t -q -p "$T" -c "$T/t.conf" 2>&1); then
  rm -rf "$T"; restore
  die "自检失败，已恢复原状：$out"
fi
rm -rf "$T" "$dest.old"
echo "syedge-lua $pkg 已安装（$base），模块目录 $dest"
echo "节点 agent 会自动上报；控制台此后允许向该节点发布含 JWT 规则的配置。"
