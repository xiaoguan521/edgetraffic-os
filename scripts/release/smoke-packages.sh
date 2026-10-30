#!/usr/bin/env bash
# 软件包冒烟测试：在多个发行版容器里安装 RPM / DEB，验证依赖可解析、二进制能在该发行版的 glibc 上运行、
# 安装脚本写入初始配置且 syapp 能以基线配置启动。容器内没有 systemd，安装脚本会跳过 systemctl。
#
# 用法：scripts/release/smoke-packages.sh <软件包目录> <goarch>
# 环境变量：SMOKE_SET=full（默认，main 与正式发布）| pr（拉取请求：代表性子集）；SMOKE_JOBS 并行数（默认 6）
# 发行版覆盖（full）：CentOS 7（glibc 2.17）、Rocky 8（2.28，与麒麟 V10、统信 UOS 20 相同）、Rocky 9、openEuler 22.03、
#                     Debian 12 / 13、Ubuntu 20.04 / 22.04 / 24.04，以及 Rocky 9 / Debian 12 的独立模式。
#              （pr）：CentOS 7（最老 glibc）、Rocky 9、Ubuntu 24.04，以及两个独立模式场景。
# 各场景在独立容器中并行运行，结果按固定顺序输出。
set -uo pipefail
DIR=$(realpath "${1:?package dir}"); ARCH=${2:?goarch}
SET=${SMOKE_SET:-full}; JOBS=${SMOKE_JOBS:-6}; LIMIT=${SMOKE_TIMEOUT:-600}
case "$SET" in full|pr) ;; *) echo "SMOKE_SET 只能是 full 或 pr" >&2; exit 2 ;; esac
W=$(mktemp -d)
# 每个场景一个命名容器、限时运行：超时则强制删除容器，只判这个场景失败，不会拖住整个任务
dockerrun() { # 场景编号 docker run 参数……
  local id=$1; shift
  local name="etos-smoke-$$-$id"
  timeout --kill-after=20 "$LIMIT" docker run --rm --name "$name" "$@"
  local rc=$?
  if [[ $rc -eq 124 || $rc -eq 137 ]]; then
    docker rm -f "$name" >/dev/null 2>&1
    echo "超时：超过 ${LIMIT}s 仍未完成"
  fi
  return $rc
}
run() { # 发行版名 镜像 安装命令
  local name=$1 image=$2 install=$3 out
  out=$(dockerrun "$SCENARIO" --platform "linux/$ARCH" -v "$DIR:/pkgs:ro" "$image" sh -c "
    set -e
    $install
    syapp -v 2>&1
    test -x /usr/libexec/syapp/syapp-wrapper
    test -x /usr/sbin/syapp-firewall && test -f /usr/lib/systemd/system/syapp-firewall.service
    test ! -e /etc/syapp/firewall.nft   # 防火墙默认不启用
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
    return 1
  fi
}
deb='export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq /pkgs/syapp_*.deb /pkgs/syapp-agent_*.deb /pkgs/syapp-console_*.deb >/dev/null'
dnf='dnf install -y -q /pkgs/syapp-[0-9]*.rpm /pkgs/syapp-agent-*.rpm /pkgs/syapp-console-*.rpm >/dev/null'
# openEuler 默认启用 everything / EPOL / update 等大仓库，从 CI 所在地区拉取元数据常常超过场景超时：
# 只启用 OS 基础仓库（依赖都在其中），并对慢速镜像设置超时与最低速率，失败重试而不是卡住
dnf_oe='dnf install -y -q --disablerepo="*" --enablerepo=OS --setopt=install_weak_deps=False --setopt=timeout=30 --setopt=minrate=20k --setopt=retries=3 /pkgs/syapp-[0-9]*.rpm /pkgs/syapp-agent-*.rpm /pkgs/syapp-console-*.rpm >/dev/null'
# CentOS 7 的官方软件源已下线：只安装本产品的软件包（sudo 依赖跳过），重点验证 glibc 2.17 兼容
el7='rpm -i /pkgs/syapp-[0-9]*.rpm && rpm -i --nodeps /pkgs/syapp-agent-*.rpm /pkgs/syapp-console-*.rpm'

# 独立模式：只装 syapp 软件包，按说明加一个站点后能直接服务；之后装 syapp-agent，
# 注册时因为有本机自定义配置而被拒绝（不会被控制台静默替换）。
standalone() { # 发行版名 镜像 安装 syapp 的命令 安装 agent 的命令
  local name=$1 image=$2 install=$3 agent=$4 out
  out=$(dockerrun "$SCENARIO" --platform "linux/$ARCH" -v "$DIR:/pkgs:ro" "$image" sh -c "
    set -e
    $install
    test \"\$(readlink /opt/syapp/conf/current)\" = releases/r000000
    test -f /opt/syapp/share/doc/STANDALONE.txt
    /opt/syapp/sbin/syapp -t -q
    cp /opt/syapp/share/defaults/standalone-example.conf /opt/syapp/conf/current/conf.d/www.conf
    /opt/syapp/sbin/syapp -t -q
    /opt/syapp/sbin/syapp
    sleep 1
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: www.example.com' http://127.0.0.1/)\" = 200
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: other.example.com' http://127.0.0.1/ || true)\" = 000
    sed -i 's/www.example.com;/www.example.com shop.example.com;/' /opt/syapp/conf/current/conf.d/www.conf
    /opt/syapp/sbin/syapp -t -q && /opt/syapp/sbin/syapp -s reload
    sleep 1
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: shop.example.com' http://127.0.0.1/)\" = 200
    $agent
    test \"\$(readlink /opt/syapp/conf/current)\" = releases/r000000
    test -f /opt/syapp/conf/current/conf.d/www.conf
    if timeout 60 syapp-agent enroll -server 127.0.0.1:1 -token t -ca-fingerprint f 2>/tmp/e; then echo enroll-accepted; exit 1; fi
    grep -q replace-local-config /tmp/e
    echo standalone-ok
  " 2>&1)
  if [[ $? -eq 0 && "$out" == *standalone-ok* ]]; then
    echo "ok   $name 独立模式：独立服务、reload、接入控制台前的保护"
  else
    echo "FAIL $name 独立模式"
    echo "$out" | tail -15 | sed 's/^/     /'
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=smoke-packages standalone $name::$(echo "$out" | tail -6 | tr '\n' ' ')"
    return 1
  fi
}
sa_dnf='dnf install -y -q /pkgs/syapp-[0-9]*.rpm >/dev/null'
sa_dnf_agent='dnf install -y -q /pkgs/syapp-agent-*.rpm >/dev/null'
sa_deb='export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq curl /pkgs/syapp_*.deb >/dev/null'
sa_deb_agent='apt-get install -y -qq /pkgs/syapp-agent_*.deb >/dev/null'

# 场景表：集合（full = 只在完整集合中；pr = 两个集合都包含）| 函数 | 参数……
scenarios=(
  "pr|run|CentOS 7|centos:7|$el7"
  "full|run|Rocky Linux 8|rockylinux:8|$dnf"
  "pr|run|Rocky Linux 9|rockylinux:9|$dnf"
  "full|run|openEuler 22.03|openeuler/openeuler:22.03-lts|$dnf_oe"
  "full|run|Debian 13|debian:13|$deb"
  "full|run|Debian 12|debian:12|$deb"
  "full|run|Ubuntu 20.04|ubuntu:20.04|$deb"
  "full|run|Ubuntu 22.04|ubuntu:22.04|$deb"
  "pr|run|Ubuntu 24.04|ubuntu:24.04|$deb"
  "pr|standalone|Rocky Linux 9|rockylinux:9|$sa_dnf|$sa_dnf_agent"
  "pr|standalone|Debian 12|debian:12|$sa_deb|$sa_deb_agent"
)

echo "== 软件包冒烟（$ARCH，集合 $SET，并行 $JOBS，每个场景限时 ${LIMIT}s）"
start=$SECONDS
i=0; selected=()
for sc in "${scenarios[@]}"; do
  IFS='|' read -r set fn a1 a2 a3 a4 <<<"$sc"
  [[ "$SET" == pr && "$set" != pr ]] && continue
  i=$((i+1)); selected+=("$i")
  while (( $(jobs -rp | wc -l) >= JOBS )); do wait -n; done
  ( t0=$SECONDS; SCENARIO=$i
    if [[ $fn == run ]]; then run "$a1" "$a2" "$a3"; else standalone "$a1" "$a2" "$a3" "$a4"; fi > "$W/$i.out" 2>&1
    echo $? > "$W/$i.rc"; echo $((SECONDS-t0)) > "$W/$i.sec" ) &
done
wait
fail=0
for i in "${selected[@]}"; do
  sed "1s/\$/  ($(cat "$W/$i.sec")s)/" "$W/$i.out"
  [[ "$(cat "$W/$i.rc")" == 0 ]] || fail=$((fail+1))
done
echo "---- ${#selected[@]} 个场景，用时 $((SECONDS-start))s"
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "PACKAGE SMOKE OK" || exit 1
