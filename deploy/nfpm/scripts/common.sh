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
is_remove() { [ "$1" = "0" ] || [ "$1" = "remove" ] || [ "$1" = "purge" ]; }
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
