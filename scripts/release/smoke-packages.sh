#!/usr/bin/env bash
# 软件包冒烟测试：在多个发行版容器里安装 RPM / DEB，验证依赖可解析、二进制能在该发行版的 glibc 上运行、
# 安装脚本写入初始配置且 syedge 能以基线配置启动。容器内没有 systemd，安装脚本会跳过 systemctl。
#
# 用法：scripts/release/smoke-packages.sh <软件包目录> <goarch>
# 环境变量：SMOKE_SET=full（默认）| pr；SMOKE_JOBS 并行数（默认 6）；
# 不再测试从 v0.4.2 及更早版本顺序升级：运行文件布局改为统一放在实例前缀下，旧版节点需要重装。
# SMOKE_FILTER 可按场景函数和名称筛选，CI 默认不筛选。
# 发行版覆盖（full）：CentOS 7（glibc 2.17）、Rocky 8（2.28，与麒麟 V10、统信 UOS 20 相同）、Rocky 9、openEuler 22.03、
#                     Debian 12 / 13、Ubuntu 20.04 / 22.04 / 24.04，以及 Rocky 9 / Debian 12 的独立模式。
#              （pr）：CentOS 7（最老 glibc）、Rocky 9、Ubuntu 24.04，以及两个独立模式场景。
# 各场景在独立容器中并行运行，结果按固定顺序输出。
set -uo pipefail
DIR=$(realpath "${1:?package dir}"); ARCH=${2:?goarch}
SET=${SMOKE_SET:-full}; JOBS=${SMOKE_JOBS:-6}; LIMIT=${SMOKE_TIMEOUT:-600}
DEFAULT_ROOT=${SYEDGE_PREFIX:-/opt/syedge}
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
run() { # 发行版名 镜像 安装命令 [自定义安装根目录]
  local name=$1 image=$2 install=$3 prefix=${4:-} out root=$DEFAULT_ROOT dp_opts='' prefix_setup=: prefix_mode_check=:
  local base_root=$DEFAULT_ROOT
  if [[ -n "$prefix" ]]; then
    root=$prefix
    prefix_setup="mkdir -p /etc/syedge; install -d -m 0751 '$prefix'; printf '%s\\n' '$prefix' > /etc/syedge/prefix"
    prefix_mode_check="test \"\$(stat -c %a '$prefix')\" = 751"
  fi
  # 运维的日常用法：只给实例前缀，不带 -c、-e（配置、日志、pid 都编译为相对 -p 的路径）
  dp_opts="-p $root"
  out=$(dockerrun "$SCENARIO" --platform "linux/$ARCH" -e "SYEDGE_PREFIX=$base_root" -v "$DIR:/pkgs:ro" "$image" sh -c "
    set -e
    $prefix_setup
    $install
    $prefix_mode_check
    syedge -v 2>&1
    test -x /usr/libexec/syedge/syedge-wrapper
    test -x /usr/libexec/syedge/syedge-instance
    test -x /usr/sbin/syedge-firewall && test -f /usr/lib/systemd/system/syedge-firewall.service
    test ! -e /var/lib/syedge-firewall/firewall.nft   # 防火墙默认不启用
    id syedge-agent >/dev/null && id syedge-console >/dev/null
    test \"\$(syedge-instance prefix)\" = '$root'
    test -L '$root/conf/current'
    test -f /etc/logrotate.d/syedge
    '$root/sbin/syedge' $dp_opts -t -q
    '$root/sbin/syedge' $dp_opts
    sleep 1
    grep -qs 'SyEdge: master process' /proc/[0-9]*/cmdline
    test -f /etc/syedge-console/console.env
    syedge-console -version
    test ! -e /usr/bin/syedge-license-server
  " 2>&1)
  if [[ $? -eq 0 ]]; then
    echo "ok   $name: $(echo "$out" | grep -m1 'SyEdge version')"
  else
    echo "FAIL $name"
    echo "$out" | tail -50 | sed 's/^/     /'
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=smoke-packages $name::$(echo "$out" | tail -6 | tr '\n' ' ')"
    return 1
  fi
}
deb='export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq /pkgs/syedge_*.deb /pkgs/syedge-agent_*.deb /pkgs/syedge-console_*.deb >/dev/null'
dnf='dnf install -y -q /pkgs/syedge-[0-9]*.rpm /pkgs/syedge-agent-*.rpm /pkgs/syedge-console-*.rpm >/dev/null'
# openEuler 默认启用 everything / EPOL / update 等大仓库，从 CI 所在地区拉取元数据常常超过场景超时：
# 只启用 OS 基础仓库（依赖都在其中），并对慢速镜像设置超时与最低速率，失败重试而不是卡住
dnf_oe='dnf install -y -q --disablerepo="*" --enablerepo=OS --setopt=install_weak_deps=False --setopt=timeout=30 --setopt=minrate=20k --setopt=retries=3 /pkgs/syedge-[0-9]*.rpm /pkgs/syedge-agent-*.rpm /pkgs/syedge-console-*.rpm'
# CentOS 7 的官方软件源已下线：只安装本产品的软件包（sudo 依赖跳过），重点验证 glibc 2.17 兼容
el7='rpm -i /pkgs/syedge-[0-9]*.rpm && rpm -i --nodeps /pkgs/syedge-agent-*.rpm /pkgs/syedge-console-*.rpm'

# 独立模式：只装 syedge 软件包，按说明加一个站点后能直接服务；之后装 syedge-agent，
# 注册时因为有本机自定义配置而被拒绝（不会被控制台静默替换）。
standalone() { # 发行版名 镜像 安装 syedge 的命令 安装 agent 的命令 [自定义根目录]
  local name=$1 image=$2 install=$3 agent=$4 prefix=${5:-} out root=$DEFAULT_ROOT dp_opts='' prefix_setup=:
  local base_root=$DEFAULT_ROOT
  if [[ -n "$prefix" ]]; then
    root=$prefix
    prefix_setup="mkdir -p /etc/syedge; printf '%s\\n' '$prefix' > /etc/syedge/prefix; umask 077"
  fi
  # 运维的日常用法：只给实例前缀，不带 -c、-e（配置、日志、pid 都编译为相对 -p 的路径）
  dp_opts="-p $root"
  out=$(dockerrun "$SCENARIO" --platform "linux/$ARCH" -e "SYEDGE_PREFIX=$base_root" -v "$DIR:/pkgs:ro" "$image" sh -c "
    set -ex
    $prefix_setup
    $install
    umask 022
    test \"\$(readlink '$root/conf/current')\" = releases/r000000
    test -f '$root/share/doc/STANDALONE.txt'
    grep -Fq '$root/sbin/syedge -p $root' '$root/share/doc/STANDALONE.txt'
    '$root/sbin/syedge' $dp_opts -t -q
    cp '$root/share/defaults/standalone-example.conf' '$root/conf/current/conf.d/www.conf'
    if [ '$root' != '$base_root' ]; then printf 'standalone-content-preserved\n' > '$root/html/index.html'; fi
    '$root/sbin/syedge' $dp_opts -t -q
    '$root/sbin/syedge' $dp_opts
    sleep 1
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: www.example.com' http://127.0.0.1/)\" = 200
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: other.example.com' http://127.0.0.1/ || true)\" = 000
    sed -i 's/www.example.com;/www.example.com shop.example.com;/' '$root/conf/current/conf.d/www.conf'
    '$root/sbin/syedge' $dp_opts -t -q && '$root/sbin/syedge' $dp_opts -s reload
    sleep 1
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: shop.example.com' http://127.0.0.1/)\" = 200
    printf '# smoke-logrotate-policy\n' >> /etc/logrotate.d/syedge
    ( for n in \$(seq 1 50); do
        curl -fsS -m 3 -H 'Host: shop.example.com' http://127.0.0.1/ >/dev/null || exit 1
        sleep 0.1
      done ) &
    probe=\$!
    if [ '$root' != '$base_root' ]; then umask 077; fi
    if command -v rpm >/dev/null 2>&1; then
      rpm -U --replacepkgs --oldpackage /pkgs/syedge-[0-9]*.rpm
    else
      dpkg -i --force-downgrade /pkgs/syedge_*.deb
    fi
    umask 022
    wait \"\$probe\"
    kill -0 \"\$(cat '$root/run/syedge.pid')\"
    grep -q smoke-logrotate-policy /etc/logrotate.d/syedge
    test \"\$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: shop.example.com' http://127.0.0.1/)\" = 200
    if [ '$root' != '$base_root' ]; then
      test \"\$(cat '$root/html/index.html')\" = standalone-content-preserved
      curl -s -m 5 -H 'Host: shop.example.com' http://127.0.0.1/ | grep -q standalone-content-preserved
    fi
    if [ '$root' != '$base_root' ]; then umask 077; fi
    $agent
    umask 022
    test \"\$(readlink '$root/conf/current')\" = releases/r000000
    test -f '$root/conf/current/conf.d/www.conf'
    grep -q 'shop.example.com' '$root/conf/current/conf.d/www.conf'
    if timeout 60 syedge-agent enroll -server 127.0.0.1:1 -token t -ca-fingerprint f 2>/tmp/e; then echo enroll-accepted; exit 1; fi
    grep -q replace-local-config /tmp/e
    if command -v rpm >/dev/null 2>&1; then rpm -e syedge-agent; else dpkg -r syedge-agent; fi
    test -f '$root/conf/current/conf.d/www.conf'
    if command -v rpm >/dev/null 2>&1; then rpm -e syedge; else dpkg -r syedge; fi
    for n in \$(seq 1 30); do
      code=\$(curl -s -m 1 -o /dev/null -w '%{http_code}' -H 'Host: shop.example.com' http://127.0.0.1/ || true)
      [ \"\$code\" = 000 ] && break
      sleep 0.1
    done
    test \"\$code\" = 000
    echo standalone-ok
  " 2>&1)
  if [[ $? -eq 0 && "$out" == *standalone-ok* ]]; then
    echo "ok   $name 独立模式：独立服务、reload、接入控制台前的保护"
  else
    echo "FAIL $name 独立模式"
    echo "$out" | tail -25 | sed 's/^/     /'
    [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=smoke-packages standalone $name::$(echo "$out" | tail -6 | tr '\n' ' ')"
    return 1
  fi
}

sa_dnf='dnf install -y -q /pkgs/syedge-[0-9]*.rpm >/dev/null'
sa_dnf_agent='dnf install -y -q /pkgs/syedge-agent-*.rpm >/dev/null'
sa_deb='export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq curl /pkgs/syedge_*.deb >/dev/null'
sa_deb_agent='apt-get install -y -qq /pkgs/syedge-agent_*.deb >/dev/null'

# 场景表：集合（full = 只在完整集合中；pr = 两个集合都包含）| 函数 | 参数……
scenarios=(
  "pr|run|CentOS 7|centos:7|$el7"
  "full|run|Rocky Linux 8|rockylinux:8|$dnf"
  "pr|run|Rocky Linux 9|rockylinux:9|$dnf"
  "pr|run|openEuler 22.03|openeuler/openeuler:22.03-lts|$dnf_oe"
  "full|run|Debian 13|debian:13|$deb"
  "full|run|Debian 12|debian:12|$deb"
  "full|run|Ubuntu 20.04|ubuntu:20.04|$deb"
  "full|run|Ubuntu 22.04|ubuntu:22.04|$deb"
  "pr|run|Ubuntu 24.04|ubuntu:24.04|$deb"
  "pr|run|Rocky Linux 9 custom prefix|rockylinux:9|$dnf||/srv/syedge-smoke"
  "pr|run|openEuler 22.03 custom prefix|openeuler/openeuler:22.03-lts|$dnf_oe||/home/atwasoft/node"
  "pr|standalone|Rocky Linux 9|rockylinux:9|$sa_dnf|$sa_dnf_agent"
  "pr|standalone|Debian 12|debian:12|$sa_deb|$sa_deb_agent"
  "pr|standalone|Rocky Linux 9 custom prefix|rockylinux:9|$sa_dnf|$sa_dnf_agent|/srv/syedge-smoke"
  "pr|standalone|Debian 12 custom prefix|debian:12|$sa_deb|$sa_deb_agent|/srv/syedge-smoke"
  "pr|standalone|Debian 12 home prefix|debian:12|$sa_deb|$sa_deb_agent|/home/atwasoft/node"
)
echo "== 软件包冒烟（${ARCH}，集合 ${SET}，并行 ${JOBS}，每个场景限时 ${LIMIT}s）"
start=$SECONDS
i=0; selected=()
for sc in "${scenarios[@]}"; do
  IFS='|' read -r set fn a1 a2 a3 a4 a5 <<<"$sc"
  [[ "$SET" == pr && "$set" != pr ]] && continue
  [[ -z "${SMOKE_FILTER:-}" || "$fn $a1" == *"$SMOKE_FILTER"* ]] || continue
  i=$((i+1)); selected+=("$i")
  while (( $(jobs -rp | wc -l) >= JOBS )); do wait -n; done
  ( t0=$SECONDS; SCENARIO=$i
    if [[ $fn == run ]]; then run "$a1" "$a2" "$a3" "$a5"; else "$fn" "$a1" "$a2" "$a3" "$a4" "$a5"; fi > "$W/$i.out" 2>&1
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
