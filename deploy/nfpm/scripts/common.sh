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
