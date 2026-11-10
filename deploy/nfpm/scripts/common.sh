# 软件包脚本共用函数（打包时内联到每个脚本开头，不单独安装）。
# 阶段判断兼容 RPM 与 DEB：
#   RPM %post $1: 1=安装 2=升级；%preun $1: 0=卸载 1=升级
#   DEB postinst $1=configure $2=旧版本（为空表示全新安装）；prerm $1=remove|upgrade
has_systemd() { [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; }
# 注意：函数不能叫 nologin，否则 command -v 会找到函数自己而不是 /sbin/nologin
nologin_shell() {
  for s in /usr/sbin/nologin /sbin/nologin; do [ -x "$s" ] && { echo "$s"; return; }; done
  echo /bin/false
}
ensure_group() { getent group "$1" >/dev/null 2>&1 || groupadd -r "$1"; }
ensure_user() { id "$1" >/dev/null 2>&1 || useradd -r -g "$2" -d /nonexistent -M -s "$(nologin_shell)" "$1"; }
LEGACY_ROOT=/opt/"syedge"
PACKAGE_ROOT=/opt/syedge
is_remove() { [ "$1" = "0" ] || [ "$1" = "remove" ] || [ "$1" = "purge" ]; }
# 安装包总会解压到编译前缀，即使实际运行目录是旧路径或自定义路径。
# 在包管理器写入前检查该前缀及其父目录，避免经可写目录或符号链接替换文件。
check_package_root() {
  package_dir=$1
  check_path=$package_dir
  trusted_child=0
  while :; do
    [ ! -L "$check_path" ] || { echo "syedge: 软件包目录不能包含符号链接：$check_path" >&2; return 1; }
    if [ -e "$check_path" ]; then
      [ -d "$check_path" ] || { echo "syedge: 软件包路径不是目录：$check_path" >&2; return 1; }
      check_stat=$(stat -c '%u %a' "$check_path") || return 1
      set -- $check_stat
      [ "$1" = 0 ] || { echo "syedge: 软件包目录必须属于 root：$check_path" >&2; return 1; }
      if [ $((0$2 & 022)) -ne 0 ]; then
        if [ "$check_path" != /home ] || [ $((0$2 & 01000)) -eq 0 ] || [ "$trusted_child" -ne 1 ]; then
          echo "syedge: 软件包目录及父目录不能被其他用户写入：$check_path" >&2
          return 1
        fi
      fi
      trusted_child=1
    else
      trusted_child=0
    fi
    [ "$check_path" != / ] || break
    check_path=${check_path%/*}
    [ -n "$check_path" ] || check_path=/
  done
  for check_path in "$package_dir/sbin" "$package_dir/share" "$package_dir/html" "$package_dir/modules"; do
    [ ! -L "$check_path" ] || { echo "syedge: 软件包目录不能是符号链接：$check_path" >&2; return 1; }
  done
}
syedge_root() {
  # 数据面包自带当前版本的校验器；升级时 /usr/sbin 中的 agent 工具可能仍是旧版。
  if [ -x /usr/libexec/syedge/syedge-instance ]; then
    /usr/libexec/syedge/syedge-instance prefix || return 1
  elif [ ! -e /etc/syedge/prefix ] && [ ! -L /etc/syedge/prefix ]; then
    echo /opt/syedge
  elif [ -x /usr/sbin/syedge-instance ]; then
    /usr/sbin/syedge-instance prefix || return 1
  else
    echo "syedge: 无法校验 /etc/syedge/prefix" >&2
    return 1
  fi
}
# RPM 在 %post 后还会清理旧包文件。前缀变更时恢复实际安装目录中被清走的
# 文件；只补缺失文件，不覆盖管理员维护的内容或已安装的数据面版本。
restore_package_payload() {
  restore_root=$1
  [ "$restore_root" != "$PACKAGE_ROOT" ] || return 0
  [ -x "$PACKAGE_ROOT/sbin/syedge" ] || { echo "syedge: 软件包数据面不存在" >&2; return 1; }
  for d in "$restore_root/sbin" "$restore_root/share" "$restore_root/html"; do
    [ ! -L "$d" ] || { echo "syedge: 安装目录不能是符号链接：$d" >&2; return 1; }
    mkdir -p "$d"
  done
  if [ ! -x "$restore_root/sbin/syedge" ]; then
    tmp=$(mktemp "$restore_root/.syedge-bin.XXXXXX") || return 1
    if ! install -o root -g root -m 0755 "$PACKAGE_ROOT/sbin/syedge" "$tmp" || ! mv -f "$tmp" "$restore_root/sbin/syedge"; then
      rm -f "$tmp"
      return 1
    fi
  fi
  for restore_guide in share/defaults/standalone-example.conf share/doc/STANDALONE.txt; do
    if [ ! -e "$restore_root/$restore_guide" ] && [ ! -L "$restore_root/$restore_guide" ] && [ -f "$PACKAGE_ROOT/$restore_guide" ]; then
      mkdir -p "$(dirname "$restore_root/$restore_guide")"
      sed "s|$PACKAGE_ROOT|$restore_root|g" "$PACKAGE_ROOT/$restore_guide" > "$restore_root/$restore_guide"
      chmod 0644 "$restore_root/$restore_guide"
    fi
  done
  [ ! -d "$PACKAGE_ROOT/share" ] || cp -an "$PACKAGE_ROOT/share/." "$restore_root/share/"
  [ ! -d "$PACKAGE_ROOT/html" ] || cp -an "$PACKAGE_ROOT/html/." "$restore_root/html/"
  chown syedge:syedge "$restore_root/sbin"
}
# 同机多实例（syedge-instance add 创建）：卸载软件包前必须先逐个删除实例，否则拒绝卸载（设计 0001 第 5.7 节）
named_instances() {
  r=$(syedge_root) || return 1
  for f in "$r"/instances/*/instance.json; do
    if [ -f "$f" ]; then basename "$(dirname "$f")" || return 1; fi
  done
  return 0
}
refuse_remove_with_instances() {
  is_remove "$1" || return 0
  [ "${SYEDGE_FORCE_REMOVE:-}" = "1" ] && return 0
  insts=$(named_instances) || return 1
  [ -z "$insts" ] && return 0
  echo "本机还有同机多实例：$insts" >&2
  echo "请先逐个执行 syedge-instance remove <实例名> --yes，再卸载软件包；确需强制卸载请设置 SYEDGE_FORCE_REMOVE=1" >&2
  exit 1
}
