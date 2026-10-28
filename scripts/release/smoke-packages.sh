#!/usr/bin/env bash
# 软件包冒烟测试：在多个发行版容器里安装 RPM / DEB，验证依赖可解析、二进制能在该发行版的 glibc 上运行、
# 安装脚本写入初始配置且 syapp 能以基线配置启动。容器内没有 systemd，安装脚本会跳过 systemctl。
#
# 用法：scripts/release/smoke-packages.sh <软件包目录> <goarch>
# 发行版覆盖：CentOS 7（glibc 2.17）、Rocky 8（2.28，与麒麟 V10、统信 UOS 20 相同）、Rocky 9、openEuler 22.03、
#             Debian 11 / 12、Ubuntu 20.04 / 22.04 / 24.04。
set -uo pipefail
DIR=$(realpath "${1:?package dir}"); ARCH=${2:?goarch}
fail=0
run() { # 发行版名 镜像 安装命令
  local name=$1 image=$2 install=$3 out
  out=$(docker run --rm --platform "linux/$ARCH" -v "$DIR:/pkgs:ro" "$image" sh -c "
    set -e
    $install
    syapp -v 2>&1
    test -x /usr/libexec/syapp/syapp-wrapper
    id syapp-agent >/dev/null && id syapp-console >/dev/null
    test -L /opt/syapp/conf/current
    /opt/syapp/sbin/syapp -t -q
    /opt/syapp/sbin/syapp
    sleep 1
    grep -qs 'syapp: master process' /proc/[0-9]*/cmdline
    test -f /etc/syapp-console/console.env
    syapp-console -version
    test ! -e /usr/bin/syapp-license-server
  " 2>&1)
  if [[ $? -eq 0 ]]; then
    echo "ok   $name: $(echo "$out" | grep -m1 'syapp version')"
  else
    echo "FAIL $name"
    echo "$out" | tail -15 | sed 's/^/     /'
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=smoke-packages $name::$(echo "$out" | tail -6 | tr '\n' ' ')"
    fail=$((fail+1))
  fi
}
deb='export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq /pkgs/syapp_*.deb /pkgs/syapp-agent_*.deb /pkgs/syapp-console_*.deb >/dev/null'
dnf='dnf install -y -q /pkgs/syapp-[0-9]*.rpm /pkgs/syapp-agent-*.rpm /pkgs/syapp-console-*.rpm >/dev/null'
# CentOS 7 的官方软件源已下线：只安装本产品的软件包（sudo 依赖跳过），重点验证 glibc 2.17 兼容
el7='rpm -i /pkgs/syapp-[0-9]*.rpm && rpm -i --nodeps /pkgs/syapp-agent-*.rpm /pkgs/syapp-console-*.rpm'

run "CentOS 7"         centos:7                   "$el7"
run "Rocky Linux 8"    rockylinux:8               "$dnf"
run "Rocky Linux 9"    rockylinux:9               "$dnf"
run "openEuler 22.03"  openeuler/openeuler:22.03-lts "$dnf"
run "Debian 11"        debian:11                  "$deb"
run "Debian 12"        debian:12                  "$deb"
run "Ubuntu 20.04"     ubuntu:20.04               "$deb"
run "Ubuntu 22.04"     ubuntu:22.04               "$deb"
run "Ubuntu 24.04"     ubuntu:24.04               "$deb"

echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "PACKAGE SMOKE OK" || exit 1
