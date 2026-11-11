#!/usr/bin/env bash
# syedge-lua 附加包安装（P10）：在节点上以 root 执行  bash install.sh
# - 附加包必须与运行中的 syedge 版本一致（动态模块与二进制版本强绑定）；
# - 自动选择标准版或国密版对应的模块；
# - 安装到安装根目录下的 modules/lua-<包版本>/，再原子切换软链 modules/lua；
# - 用真实 syedge -t 自检，失败则恢复原状。不重启、不停止 syedge，也不改动任何已发布的配置。
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "syedge-lua: $*" >&2; exit 1; }

packaged_default_root() {
  local root=/opt/syedge
  if [[ -f "$HERE/addon/PREFIX" ]]; then
    IFS= read -r root < "$HERE/addon/PREFIX" || die "无法读取附加包默认目录"
  fi
  [[ "$root" =~ ^(/[A-Za-z0-9._@+-]+)+$ && "$root" != */../* && "$root" != */.. && "$root" != */./* && "$root" != */. ]] || die "附加包默认目录无效"
  printf '%s\n' "$root"
}

# 独立模式 Lua 的动作：空 / enable / disable；只属于 install.sh，由 resolve_target 解析。
STANDALONE_ACTION=""
# 安装和卸载共用目标选择，供同包的 uninstall.sh source；不执行任何安装操作。
resolve_target() {
  local requested="${SYEDGE_INSTANCE:-}" result name mode dir i choice act
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --instance) [[ $# -ge 2 ]] || die "--instance 需要实例名"; requested="$2"; shift 2 ;;
      --instance=*) requested="${1#*=}"; shift ;;
      --standalone|--standalone-disable)
        act=enable
        if [[ "$1" == --standalone-disable ]]; then act=disable; fi
        if [[ -n "$STANDALONE_ACTION" && "$STANDALONE_ACTION" != "$act" ]]; then die "--standalone 与 --standalone-disable 互斥"; fi
        STANDALONE_ACTION=$act; shift ;;
      -h|--help) echo "用法：bash $(basename "$0") [--instance <实例名或 default>] [--standalone | --standalone-disable]（后两个只用于 install.sh：独立模式启用 / 停用 Lua）"; exit 0 ;;
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
    # 发布包携带构建时前缀；sudo 默认不会保留构建环境变量。
    BASE_ROOT=${SYEDGE_PREFIX:-}
    if [[ -z "$BASE_ROOT" ]]; then BASE_ROOT=$(packaged_default_root) || die "无法识别附加包默认目录"; fi
    if [[ "$BASE_ROOT" != /opt/syedge && -x /opt/syedge/sbin/syedge ]]; then
      [[ ! -x "$BASE_ROOT/sbin/syedge" ]] || die "检测到多个安装目录，请安装 syedge-instance 或指定 SYEDGE_ROOT"
      BASE_ROOT=/opt/syedge
    fi
  else
    die "缺少安装根目录校验工具"
  fi
  [[ "$BASE_ROOT" =~ ^(/[A-Za-z0-9._@+-]+)+$ && "$BASE_ROOT" != */../* && "$BASE_ROOT" != */.. && "$BASE_ROOT" != */./* && "$BASE_ROOT" != */. ]] || die "安装根目录无效"
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

# ---- 独立模式 Lua（--standalone 启用，--standalone-disable 停用）----
# 只改两处：主配置最前面的两行 load_module，以及 conf.d/00-lua-standalone.conf。不 reload、不 restart、不停止数据面。
# IPREFIX 是目标实例的前缀（默认实例为 ROOT；具名实例为 <BASE_ROOT>/instances/<实例>）。
IPREFIX="$ROOT"
if [[ "$SELECTED" != default ]]; then IPREFIX="$BASE_ROOT/instances/$SELECTED"; fi
SA_REAL=""; SA_DIR=""; SA_NEW=""; SA_BAK=""; SA_NBAK=""; SA_TMP=""; SA_LIB=""
SA_MAIN=0; SA_NEWMADE=0; SA_NEWGONE=0

# agent 状态目录，规则与 internal/layout/layout.go 的 Layout.AgentState 一致：
# 用户模式（SYEDGE_ROOT）与非历史根目录为 <实例前缀>/agent；历史根目录 /opt/syedge 下默认实例为
# /var/lib/syedge-agent，具名实例为 /var/lib/syedge-agent-<实例>。
agent_state_dir() {
  if [[ -z "${SYEDGE_ROOT:-}" && "$BASE_ROOT" == /opt/syedge ]]; then
    if [[ "$SELECTED" == default ]]; then echo /var/lib/syedge-agent; else echo "/var/lib/syedge-agent-$SELECTED"; fi
  else
    echo "$IPREFIX/agent"
  fi
}

# 同属性替换：REF 为参照文件，FILE 获得它的属主与权限（不原地编辑）
same_attrs() { chown --reference="$1" "$2" && chmod --reference="$1" "$2"; }
# 原子覆盖：SRC 先获得 REF 的属主与权限，再 mv 到 DST
sa_replace() { same_attrs "$3" "$1" && mv -f -- "$1" "$2"; }

# 失败恢复：主配置用备份覆盖回去，恢复或删除 00 文件，删除本次新建的 lualib 目录。附加包保持安装（未被配置引用）。
sa_rollback() {
  if [[ -n "$SA_TMP" ]]; then rm -f -- "$SA_TMP"; fi
  if [[ "$SA_MAIN" == 1 ]]; then
    sa_replace "$SA_BAK" "$SA_REAL" "$SA_REAL" || echo "syedge-lua: 警告：主配置恢复失败，备份在 $SA_BAK" >&2
  elif [[ -n "$SA_BAK" ]]; then
    rm -f -- "$SA_BAK"
  fi
  if [[ "$SA_NEWMADE" == 1 ]]; then rm -f -- "$SA_NEW"; fi
  if [[ "$SA_NEWGONE" == 1 ]]; then
    sa_replace "$SA_NBAK" "$SA_NEW" "$SA_REAL" || echo "syedge-lua: 警告：conf.d/00-lua-standalone.conf 恢复失败，备份在 $SA_NBAK" >&2
  elif [[ -n "$SA_NBAK" ]]; then
    rm -f -- "$SA_NBAK"
  fi
  if [[ -n "$SA_LIB" ]]; then rmdir -- "$SA_LIB" 2>/dev/null || true; fi
}
sa_fail() {
  sa_rollback
  if [[ "$STANDALONE_ACTION" == enable ]]; then rm -rf -- "$dest.old"; fi
  die "$1（主配置已恢复原状）"
}

# 启用前置检查：任一不满足即报错退出，且尚未改动任何文件（在安装附加包之前执行）
standalone_precheck() {
  local conf="$IPREFIX/conf/current/syedge.conf" target st
  [[ -L "$IPREFIX/conf/current" ]] || die "独立模式需要 $IPREFIX/conf/current 是软链"
  target=$(readlink -f "$IPREFIX/conf/current") || die "无法解析 conf/current"
  [[ "$(basename "$target")" == r000000 ]] || die "conf/current 指向 $(basename "$target")：只有初始版本 r000000 的节点能独立启用 Lua（已由控制台发布过配置）"
  [[ -f "$conf" ]] || die "缺少主配置：$conf"
  st=$(agent_state_dir)
  [[ ! -e "$st/node.json" && ! -L "$st/node.json" ]] || die "节点已注册到控制台（$st/node.json 存在），不能独立启用 Lua"
  if grep -Eqs '^[[:space:]]*load_module[[:space:]]' "$conf"; then die "主配置已含 load_module，独立模式 Lua 可能已启用，未做任何改动"; fi
  [[ ! -e "$IPREFIX/conf/current/conf.d/00-lua-standalone.conf" ]] || die "已存在 conf.d/00-lua-standalone.conf，独立模式 Lua 已启用，未做任何改动"
}

# 启用：主配置前面插入两行 load_module，新建 conf.d/00-lua-standalone.conf，创建 lualib，然后用真实 syedge -t 校验。
standalone_enable() {
  local conf="$IPREFIX/conf/current/syedge.conf" l1 l2 out
  l1="load_module  $ROOT/modules/lua/ndk_http_module.so;"
  l2="load_module  $ROOT/modules/lua/ngx_http_lua_module.so;"
  SA_BAK=""; SA_NBAK=""; SA_TMP=""; SA_MAIN=0; SA_NEWMADE=0; SA_NEWGONE=0; SA_LIB=""
  SA_REAL=$(readlink -f "$conf") || die "无法解析主配置的真实路径"
  SA_DIR=$(dirname "$SA_REAL")
  SA_NEW="$SA_DIR/conf.d/00-lua-standalone.conf"
  SA_BAK=$(mktemp "$SA_DIR/.syedge-conf.bak.XXXXXX") || sa_fail "无法创建备份文件"
  cp -- "$SA_REAL" "$SA_BAK" || sa_fail "无法备份主配置"
  same_attrs "$SA_REAL" "$SA_BAK" || sa_fail "无法设置备份文件的属主与权限"
  SA_TMP=$(mktemp "$SA_DIR/.syedge-conf.new.XXXXXX") || sa_fail "无法创建临时文件"
  { printf '%s\n' "$l1" "$l2"; cat -- "$SA_BAK"; } > "$SA_TMP" || sa_fail "无法生成新主配置"
  same_attrs "$SA_REAL" "$SA_TMP" || sa_fail "无法设置新主配置的属主与权限"
  SA_MAIN=1
  mv -f -- "$SA_TMP" "$SA_REAL" || sa_fail "无法写入主配置"
  SA_TMP=""
  if [[ ! -d "$SA_DIR/conf.d" ]]; then mkdir -p -- "$SA_DIR/conf.d" || sa_fail "无法创建 conf.d 目录"; fi
  [[ ! -e "$SA_NEW" ]] || sa_fail "已存在 conf.d/00-lua-standalone.conf"
  SA_TMP=$(mktemp "$SA_DIR/.syedge-new.XXXXXX") || sa_fail "无法创建临时文件"
  {
    printf '%s\n' "# 独立模式 Lua（install.sh --standalone 生成）。自己的 Lua 文件放在 $IPREFIX/lualib/ 下。"
    printf '%s\n' "lua_package_path   \"$ROOT/modules/lua/lualib/?.lua;$ROOT/modules/lua/extlib/?.lua;$IPREFIX/lualib/?.lua\";"
    printf '%s\n' "lua_package_cpath  \"$ROOT/modules/lua/lualib/?.disabled\";"
  } > "$SA_TMP" || sa_fail "无法生成 conf.d/00-lua-standalone.conf"
  same_attrs "$SA_REAL" "$SA_TMP" || sa_fail "无法设置 00 文件的属主与权限"
  mv -f -- "$SA_TMP" "$SA_NEW" || sa_fail "无法写入 conf.d/00-lua-standalone.conf"
  SA_TMP=""; SA_NEWMADE=1
  if [[ ! -d "$IPREFIX/lualib" ]]; then
    mkdir -m 0755 -- "$IPREFIX/lualib" || sa_fail "无法创建 lualib 目录"
    SA_LIB="$IPREFIX/lualib"
  fi
  if ! out=$("$SY" -p "$IPREFIX" -c "$conf" -e stderr -t -q 2>&1); then
    sa_fail "新配置校验失败：$out"
  fi
  rm -f -- "$SA_BAK"; SA_BAK=""
}

# 停用：删除与启用时写入内容完全相同的两行 load_module 与 00 文件，然后校验；失败则恢复原状。
standalone_disable() {
  local conf="$IPREFIX/conf/current/syedge.conf" l1 l2 out rc=0 had_new=0 had_load=0
  l1="load_module  $ROOT/modules/lua/ndk_http_module.so;"
  l2="load_module  $ROOT/modules/lua/ngx_http_lua_module.so;"
  [[ -f "$conf" ]] || die "缺少主配置：$conf"
  SA_BAK=""; SA_NBAK=""; SA_TMP=""; SA_MAIN=0; SA_NEWMADE=0; SA_NEWGONE=0; SA_LIB=""
  SA_REAL=$(readlink -f "$conf") || die "无法解析主配置的真实路径"
  SA_DIR=$(dirname "$SA_REAL")
  SA_NEW="$SA_DIR/conf.d/00-lua-standalone.conf"
  if [[ -e "$SA_NEW" ]]; then had_new=1; fi
  if grep -qxF -e "$l1" -e "$l2" -- "$SA_REAL"; then had_load=1; fi
  if [[ $had_new -eq 0 && $had_load -eq 0 ]]; then echo "独立模式 Lua 未启用，无需停用。"; return 0; fi
  SA_BAK=$(mktemp "$SA_DIR/.syedge-conf.bak.XXXXXX") || sa_fail "无法创建备份文件"
  cp -- "$SA_REAL" "$SA_BAK" || sa_fail "无法备份主配置"
  same_attrs "$SA_REAL" "$SA_BAK" || sa_fail "无法设置备份文件的属主与权限"
  if [[ $had_new -eq 1 ]]; then
    SA_NBAK=$(mktemp "$SA_DIR/.00-lua-standalone.bak.XXXXXX") || sa_fail "无法创建 00 文件的备份"
    cp -- "$SA_NEW" "$SA_NBAK" || sa_fail "无法备份 conf.d/00-lua-standalone.conf"
    same_attrs "$SA_REAL" "$SA_NBAK" || sa_fail "无法设置 00 文件备份的属主与权限"
  fi
  SA_TMP=$(mktemp "$SA_DIR/.syedge-conf.new.XXXXXX") || sa_fail "无法创建临时文件"
  grep -vxF -e "$l1" -e "$l2" -- "$SA_BAK" > "$SA_TMP" || rc=$?
  [[ $rc -le 1 ]] || sa_fail "无法读取主配置"
  same_attrs "$SA_REAL" "$SA_TMP" || sa_fail "无法设置新主配置的属主与权限"
  SA_MAIN=1
  mv -f -- "$SA_TMP" "$SA_REAL" || sa_fail "无法写入主配置"
  SA_TMP=""
  if [[ $had_new -eq 1 ]]; then
    SA_NEWGONE=1
    rm -f -- "$SA_NEW" || sa_fail "无法删除 conf.d/00-lua-standalone.conf"
  fi
  if ! out=$("$SY" -p "$IPREFIX" -c "$conf" -e stderr -t -q 2>&1); then
    sa_fail "站点配置仍在使用 Lua 指令，请先去掉站点里的 Lua 指令（syedge 校验：$out）"
  fi
  rm -f -- "$SA_BAK"; SA_BAK=""
  if [[ -n "$SA_NBAK" ]]; then rm -f -- "$SA_NBAK"; fi
  echo "独立模式 Lua 已停用（lualib 目录保留）。下一步（未 reload，配置尚未生效）：sudo systemctl reload $(unit_name)"
}
unit_name() { if [[ "$SELECTED" == default ]]; then echo syedge; else echo "syedge@$SELECTED"; fi; }

if [[ "$STANDALONE_ACTION" == disable ]]; then
  standalone_disable
  exit 0
fi
if [[ "$STANDALONE_ACTION" == enable ]]; then standalone_precheck; fi
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
# 附加库 extlib（D1 第一阶段，纯 Lua）：旧版附加包没有该目录，不报错。只有独立模式与原生兼容配置包的配置引用它
if [[ -d "$HERE/addon/extlib" ]]; then cp -r "$HERE/addon/extlib" "$tmp/extlib"; fi
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
rm -rf "$T"
if [[ "$STANDALONE_ACTION" == enable ]]; then standalone_enable; fi
rm -rf "$dest.old"
echo "syedge-lua $pkg 已安装（$base），模块目录 $dest"
if [[ "$STANDALONE_ACTION" == enable ]]; then
  echo "独立模式 Lua 已启用：主配置已加载附加包模块，自己的 Lua 文件放在 $IPREFIX/lualib/ 下。"
  echo "下一步（未 reload，配置尚未生效）：sudo systemctl reload $(unit_name)"
else
  echo "节点 agent 会自动上报；控制台此后允许向该节点发布含 JWT 规则的配置。"
fi
