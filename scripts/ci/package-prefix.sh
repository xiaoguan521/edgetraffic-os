#!/usr/bin/env bash
# 使用临时目录验证 RPM 事务后补齐旧安装目录，不需 root。
set -euo pipefail
scratch=$(mktemp -d)
check_scratch=$(mktemp -d "$PWD/.prefix-check.XXXXXX")
trap 'rm -rf "$scratch" "$check_scratch"' EXIT
# shellcheck source=../../deploy/nfpm/scripts/common.sh
source deploy/nfpm/scripts/common.sh
mkdir -p "$check_scratch/parent/node/sbin"
unsafe=""
stat() {
  if [[ "$3" == "$unsafe" ]]; then printf '0 777\n'; else printf '0 755\n'; fi
}
check_package_root "$check_scratch/parent/node"
unsafe="$check_scratch/parent"
if ( check_package_root "$check_scratch/parent/node" ) >/dev/null 2>&1; then
  echo 'world-writable package parent was accepted' >&2; exit 1
fi
unsafe=""
ln -s "$check_scratch" "$check_scratch/parent/node/share"
if ( check_package_root "$check_scratch/parent/node" ) >/dev/null 2>&1; then
  echo 'symlink package directory was accepted' >&2; exit 1
fi
unset -f stat
PACKAGE_ROOT="$scratch/package"
target="$scratch/installed"
mkdir -p "$PACKAGE_ROOT/sbin" "$PACKAGE_ROOT/share/defaults" "$PACKAGE_ROOT/share/doc" "$PACKAGE_ROOT/html" "$target/html"
printf 'new binary\n' > "$PACKAGE_ROOT/sbin/syedge"
chmod 0755 "$PACKAGE_ROOT/sbin/syedge"
printf 'default\n' > "$PACKAGE_ROOT/share/defaults/mime.types"
printf '%s/sbin/syedge -p %s\n' "$PACKAGE_ROOT" "$PACKAGE_ROOT" > "$PACKAGE_ROOT/share/doc/STANDALONE.txt"
printf 'package page\n' > "$PACKAGE_ROOT/html/index.html"
printf 'customer page\n' > "$target/html/index.html"
# 测试用户不必拥有切换属主的权限；文件安装逻辑仍通过系统 install 执行。
install() { command install -m 0755 "$7" "$8"; }
chown() { :; }
# BSD cp -n reports a skipped existing file as failure; GNU cp on Linux succeeds.
if [[ "$OSTYPE" == darwin* ]]; then
  cp() {
    command cp "$@" && return 0
    case "$2" in
      "$PACKAGE_ROOT/share/.") [[ -f "$target/share/defaults/mime.types" ]] ;;
      "$PACKAGE_ROOT/html/.") [[ -f "$target/html/index.html" ]] ;;
      *) return 1 ;;
    esac
  }
fi
restore_package_payload "$target"
[[ -x "$target/sbin/syedge" && $(cat "$target/sbin/syedge") == 'new binary' ]]
[[ $(cat "$target/share/defaults/mime.types") == default ]]
[[ $(cat "$target/share/doc/STANDALONE.txt") == "$target/sbin/syedge -p $target" ]]
[[ $(cat "$target/html/index.html") == 'customer page' ]]
printf 'existing version\n' > "$target/sbin/syedge"
restore_package_payload "$target"
[[ $(cat "$target/sbin/syedge") == 'existing version' ]]
rm -rf "$target/sbin"
ln -s "$scratch" "$target/sbin"
if ( restore_package_payload "$target" ) >/dev/null 2>&1; then
  echo 'symlink runtime directory was accepted' >&2; exit 1
fi
echo 'RPM custom-prefix payload recovery: OK'
