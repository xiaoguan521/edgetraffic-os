#!/usr/bin/env bash
# syedge-lua 附加包安装（P10）：在节点上以 root 执行  bash install.sh
# - 附加包必须与运行中的 syedge 版本一致（动态模块与二进制版本强绑定）；
# - 自动选择标准版或国密版对应的模块；
# - 安装到安装根目录下的 modules/lua-<包版本>/，再原子切换软链 modules/lua；
# - 用真实 syedge -t 自检，失败则恢复原状。不重启、不停止 syedge，也不改动任何已发布的配置。
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "syedge-lua: $*" >&2; exit 1; }

# 安装和卸载共用目标选择，供同包的 uninstall.sh source；不执行任何安装操作。
resolve_target() {
  local requested="${SYEDGE_INSTANCE:-}" result name mode dir i choice
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --instance) [[ $# -ge 2 ]] || die "--instance 需要实例名"; requested="$2"; shift 2 ;;
      --instance=*) requested="${1#*=}"; shift ;;
      -h|--help) echo "用法：bash $(basename "$0") [--instance <实例名或 default>]"; exit 0 ;;
      *) die "未知参数：$1" ;;
    esac
  done
  [[ -z "$requested" || "$requested" == default || "$requested" =~ ^[a-z][a-z0-9-]{0,15}$ ]] || die "实例名无效"
  local -a helper=()
  if [[ -n "${SYEDGE_ROOT:-}" ]]; then
    BASE_ROOT="$SYEDGE_ROOT"
    if [[ $(id -u) -ne 0 && -x "$BASE_ROOT/bin/syedge-instance" ]]; then
      helper=("$BASE_ROOT/bin/syedge-instance" --root "$BASE_ROOT")
    fi
  elif [[ -x /usr/libexec/syedge/syedge-instance ]]; then
    helper=(/usr/libexec/syedge/syedge-instance)
    BASE_ROOT="$("${helper[@]}" prefix)" || die "无法校验安装根目录"
  elif [[ -x /usr/sbin/syedge-instance ]]; then
    helper=(/usr/sbin/syedge-instance)
    BASE_ROOT="$("${helper[@]}" prefix)" || die "无法校验安装根目录"
  elif [[ ! -e /etc/syedge/prefix && ! -L /etc/syedge/prefix ]]; then
    BASE_ROOT=/opt/syedge
  else
    die "缺少安装根目录校验工具"
  fi
  [[ "$BASE_ROOT" == /* && "$BASE_ROOT" != / && "$BASE_ROOT" != */../* && "$BASE_ROOT" != */.. ]] || die "安装根目录无效"
  TARGET_COUNT=0; TARGET_NAMES=(); TARGET_MODES=(); TARGET_ROOTS=()
  if [[ ${#helper[@]} -gt 0 ]]; then
    result="$("${helper[@]}" runtimes 2>&1)" || die "无法列出实例，请先升级 syedge-instance：$result"
    while IFS=$'\t' read -r name mode dir; do
      [[ -n "$name" ]] || continue
      [[ ( "$name" == default || "$name" =~ ^[a-z][a-z0-9-]{0,15}$ ) && ( "$mode" == shared || "$mode" == independent ) ]] || die "实例列表无效"
      if [[ "$mode" == shared ]]; then
        [[ "$dir" == "$BASE_ROOT" ]] || die "共享实例目录无效"
      else
        [[ "$name" != default && "$dir" == "$BASE_ROOT/instances/$name" ]] || die "独立实例目录无效"
      fi
      TARGET_NAMES+=("$name"); TARGET_MODES+=("$mode"); TARGET_ROOTS+=("$dir"); TARGET_COUNT=$((TARGET_COUNT+1))
    done <<< "$result"
  fi
  if [[ "$TARGET_COUNT" -eq 0 ]]; then
    if [[ ${#helper[@]} -eq 0 ]] && compgen -G "$BASE_ROOT/instances/*/instance.json" >/dev/null; then
      die "检测到具名实例，但缺少对应的 syedge-instance 工具，无法安全选择安装目标"
    fi
    [[ -z "$requested" || "$requested" == default ]] || die "无法识别实例 $requested，请先安装新版 syedge-instance"
    TARGET_NAMES=(default); TARGET_MODES=(shared); TARGET_ROOTS=("$BASE_ROOT"); TARGET_COUNT=1
  fi
  if [[ -z "$requested" && "$TARGET_COUNT" -gt 1 ]]; then
    [[ -t 0 ]] || die "检测到多个实例，请使用 --instance <实例名或 default> 选择"
    echo "请选择 Lua 功能包的目标实例：" >&2
    for ((i=0; i<TARGET_COUNT; i++)); do
      printf '  %d) %s（%s，%s）\n' "$((i+1))" "${TARGET_NAMES[$i]}" "${TARGET_MODES[$i]}" "${TARGET_ROOTS[$i]}" >&2
    done
    printf '请输入编号：' >&2
    IFS= read -r choice || die "未选择实例"
    [[ "$choice" =~ ^[1-9][0-9]*$ && ${#choice} -le 6 && "$choice" -le "$TARGET_COUNT" ]] || die "实例编号无效"
    requested="${TARGET_NAMES[$((choice-1))]}"
  fi
  SELECTED="${requested:-${TARGET_NAMES[0]}}"; ROOT=""; TARGET_MODE=""
  for ((i=0; i<TARGET_COUNT; i++)); do
    if [[ "${TARGET_NAMES[$i]}" == "$SELECTED" ]]; then ROOT="${TARGET_ROOTS[$i]}"; TARGET_MODE="${TARGET_MODES[$i]}"; break; fi
  done
  [[ -n "$ROOT" ]] || die "找不到实例 $SELECTED"
  [[ ! -L "$ROOT" && ! -L "$ROOT/modules" && ! -L "$ROOT/sbin" ]] || die "程序或模块目录不能是符号链接"
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0
resolve_target "$@"
SY="$ROOT/sbin/syedge"

[[ $(id -u) -eq 0 || -n "${SYEDGE_ROOT:-}" ]] || die "请以 root 执行"
[[ -x "$SY" ]] || die "没有找到 $SY：请先安装 syedge 基础包"
echo "已选择实例：$SELECTED；安装根目录：$ROOT。"
[[ "$TARGET_MODE" != shared ]] || echo "该实例使用共享模块，会影响同一目录下的其他共享实例。"
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$HERE/addon/VERSION"; }
pkg=$(field version); want=$(field syedge)
[[ "$pkg" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || die "附加包版本无效"
running=$("$SY" -v 2>&1 | sed -n 's|.* version: [^/]*/\([0-9][0-9.]*\).*|\1|p')   # 与显示名无关
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
