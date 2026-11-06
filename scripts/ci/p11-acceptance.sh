#!/usr/bin/env bash
# P11 验收（设计 0001 第 10.3 节）：用真实的数据面验证同机多实例与非 root 运行。
#
#   scripts/ci/p11-acceptance.sh <数据面 tar.gz> [user|system|all]
#
#   user   （任何有 root 的 Linux 均可，不需要 systemd）RL-01、RL-02、MI-03（user 模式）、MI-07（user 模式）：
#          新建普通用户，用 user 模式节点包安装默认实例与 blue，在持续请求下做主机级热升级（第二个实例注入失败后重试）、
#          删除实例，检查没有属于 root 的数据面进程，wrapper 以 root 运行时拒绝
#   system （需要 systemd，在 CI 的数据面任务中执行）MI-01、MI-02、MI-03、MI-04、MI-05、MI-07：
#          安装节点包，创建 blue、green；单实例 reload 不影响其他实例；端口冲突被拒绝；
#          三个实例的主机级热升级（第二个实例注入失败、重试）在持续请求下零错误；跨实例 sudo 被拒绝；
#          reopen-all；删除 blue 时其他实例零错误；有具名实例时卸载被拒绝
#
# 需要 root、go、curl、python3。system 阶段会覆盖本机 /opt/syapp 与相关单元，只在一次性的测试机上运行。
# MI-06（wrapper 注入矩阵）由 go test ./internal/wrapper/ 覆盖；RL-03（重启机器）由 internal/instance 的 ensure 用例覆盖。
set -uo pipefail
TGZ=$(realpath "$1"); PHASE=${2:-all}
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
[[ $EUID -eq 0 ]] || { echo "需要 root" >&2; exit 2; }
export GOFLAGS=-mod=vendor CGO_ENABLED=0 NO_PROXY='*' no_proxy='*'
W=$(mktemp -d); chmod 755 "$W"
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=p11-acceptance::$1"; fail=$((fail+1)); }
UP=9.1.0   # 升级包版本（内容与当前二进制相同，只验证切换流程）
ARCH=$(go env GOARCH)

echo "== 准备：CI 临时升级包签名密钥、注入公钥的 wrapper、签名升级包"
go build -o "$W/lic" ./cmd/syapp-license-server || exit 1
"$W/lic" keygen -kid etos-ci-pkg-ephemeral -out "$W/keys" > "$W/keygen.txt" || exit 1
pub=$(grep -oE '"[A-Za-z0-9+/=]{44}"' "$W/keygen.txt" | tail -1 | tr -d '"')
go build -o "$W/wrapper" -ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/pkgsig.ciKey=$pub" ./cmd/syapp-wrapper || exit 1
OUT="$W/pkg" SYAPP_UPGRADE_VERSION=$UP scripts/release/package.sh syapp-upgrade "$ARCH" "$TGZ" >/dev/null || exit 1
UPKG="$W/pkg/syapp-$UP-$ARCH.tar.gz"
"$W/lic" sign-package -key "$W/keys/etos-ci-pkg-ephemeral.key" -kid etos-ci-pkg-ephemeral -in "$UPKG" -name syapp -version "$UP" -arch "$ARCH" -variant standard >/dev/null || exit 1
[[ -f "$UPKG.sig" ]] && ok "签名升级包 $UP" || { bad "升级包签名失败"; exit 1; }

# 持续请求：load_start <名称> <端口...>；load_stop <名称> 返回失败次数（请求实例状态页，由 worker 应答）
load_start() {
  local name=$1; shift
  : > "$W/load-$name.err"
  ( while [[ ! -f "$W/load-$name.stop" ]]; do
      for p in "$@"; do
        c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$p/__syapp_status")
        [[ "$c" == 200 ]] || echo "$p $c" >> "$W/load-$name.err"
      done
    done ) </dev/null >/dev/null 2>&1 &
  echo $! > "$W/load-$name.pid"
  sleep 0.5
}
# 在 $(...) 中调用（子 shell 不能 wait 父 shell 的后台进程）：轮询直到循环退出
load_stop() {
  local pid; pid=$(cat "$W/load-$1.pid")
  touch "$W/load-$1.stop"
  for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  rm -f "$W/load-$1.stop"
  wc -l < "$W/load-$1.err"
}
masterpid() { cat "$1" 2>/dev/null; }
uid_of() { awk '/^Uid:/{print $2}' "/proc/$1/status" 2>/dev/null; }
# drop <投放目录>：与 agent 下载后的文件名一致（<品牌>-<版本>.tar.gz 与 .sig）
drop() { cp "$UPKG" "$1/syapp-$UP.tar.gz"; cp "$UPKG.sig" "$1/syapp-$UP.tar.gz.sig"; }
exe() { readlink "/proc/$(masterpid "$1")/exe" 2>/dev/null | sed 's/ (deleted)$//'; }
workers() { pgrep -P "$(masterpid "$1")" 2>/dev/null | sort | tr '\n' ' '; }

user_phase() {
  echo "== user 模式（RL-01 / RL-02 / MI-03 / MI-07）"
  local U=etosacc HOMEDIR=/home/etosacc
  id "$U" >/dev/null 2>&1 || useradd -m -d "$HOMEDIR" -s /bin/bash "$U"
  local R=$HOMEDIR/edge
  pkill -u "$U" 2>/dev/null; sleep 0.5; rm -rf "$R"   # 上一次运行的残留
  local as=(runuser -u "$U" -- env HOME="$HOMEDIR" PATH=/usr/bin:/bin)
  OUT="$W/pkg" scripts/release/package.sh node-user "$ARCH" "$TGZ" >/dev/null || { bad "打 user 模式节点包失败"; return; }
  rm -rf "$W/nu" && mkdir -p "$W/nu" && tar -C "$W/nu" -xzf "$W"/pkg/edgetraffic-os-node-user-*-linux-"$ARCH".tar.gz
  local P; P=$(ls -d "$W"/nu/*/)
  install -m 0755 "$W/wrapper" "$P/bin/syapp-wrapper"   # 注入 CI 公钥的 wrapper（其余与发布包相同）
  chmod -R a+rX "$W/nu"
  out=$(cd "$P" && "${as[@]}" ./install-user.sh "$R" --http 18180 --https 18543 --status 18181 2>&1) \
    && ok "RL-01 普通用户安装默认实例（不经过 sudo）" || bad "RL-01 安装失败：$out"
  out=$("${as[@]}" "$R/bin/syapp-instance" --root "$R" add blue --http 18280 --https 18643 --status 18281 2>&1) \
    && ok "RL-01 普通用户添加实例 blue" || bad "RL-01 添加 blue 失败：$out"
  out=$("${as[@]}" "$R/bin/syapp-instance" --root "$R" add red --http 18280 --https 18743 --status 18381 2>&1) \
    && bad "MI-02 user 模式端口冲突未被拒绝" || { [[ "$out" == *冲突* ]] && ok "MI-02 user 模式端口冲突被拒绝" || bad "MI-02 拒绝原因不明：$out"; }
  out=$("${as[@]}" "$R/bin/syapp-instance" --root "$R" add low --http 80 --https 18843 --status 18481 2>&1) \
    && bad "user 模式接受了特权端口" || ok "user 模式拒绝 1024 以下端口"
  local dpid=$R/run/syapp.pid bpid=$R/instances/blue/run/syapp.pid
  curl -s --max-time 3 http://127.0.0.1:18181/__syapp_version | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
    && ok "状态服务上报实际运行版本" || bad "版本路径无响应"
  # RL-02：数据面进程都属于安装用户
  local owners; owners=$(ps -o user= -p "$(masterpid "$dpid")" -p "$(masterpid "$bpid")" $(pgrep -P "$(masterpid "$dpid")") $(pgrep -P "$(masterpid "$bpid")") | sort -u | tr '\n' ' ')
  [[ "$owners" == "$U " ]] && ok "RL-02 master 与 worker 都属于 $U，没有 root 进程" || bad "RL-02 进程属主：$owners"
  # wrapper：user 模式下以 root 运行被拒绝；以安装用户 reload 成功
  out=$("$R/bin/syapp-wrapper" reload 2>&1) && bad "user 模式 wrapper 以 root 运行未被拒绝" || ok "user 模式 wrapper 拒绝以 root 运行"
  local w0; w0=$(workers "$bpid")
  "${as[@]}" "$R/bin/syapp-wrapper" --instance blue reload >/dev/null 2>&1 && sleep 1
  [[ "$(workers "$bpid")" != "$w0" ]] && ok "安装用户经 wrapper reload blue（不经过 sudo）" || bad "reload blue 后 worker 未替换"
  # MI-07：日志被移走后 reopen 重新创建
  mv "$R/logs/error.log" "$R/logs/error.log.1"
  "${as[@]}" "$R/bin/syapp-wrapper" reopen >/dev/null 2>&1; sleep 0.5
  [[ -f "$R/logs/error.log" ]] && ok "MI-07 user 模式 reopen 重新打开日志" || bad "MI-07 reopen 后没有新的 error.log"

  echo "== user 模式主机级热升级（MI-03）：默认实例 → blue（注入失败后重试），持续请求"
  for a in "$R/agent" "$R/instances/blue/agent"; do
    "${as[@]}" mkdir -p "$a/upgrade"; drop "$a/upgrade"; chown -R "$U" "$a/upgrade"
  done
  local newbin=$R/versions/$UP/sbin/syapp
  load_start user 18181 18281
  out=$("${as[@]}" "$R/bin/syapp-wrapper" upgrade-start $UP 2>&1) || bad "默认实例 upgrade-start：$out"
  "${as[@]}" "$R/bin/syapp-wrapper" upgrade-commit >/dev/null 2>&1 || bad "默认实例 upgrade-commit"
  local b0; b0=$(masterpid "$bpid")
  out=$("${as[@]}" "$R/bin/syapp-wrapper" --instance blue upgrade-start $UP 2>&1) || bad "blue upgrade-start（软链已切换）：$out"
  "${as[@]}" "$R/bin/syapp-wrapper" --instance blue upgrade-abort >/dev/null 2>&1 || bad "blue upgrade-abort"
  [[ "$(masterpid "$bpid")" == "$b0" && "$(readlink "$R/sbin/syapp")" == "$newbin" ]] \
    && ok "blue 中止：回到原 master，共用软链保持新版本" || bad "blue 中止后 pid=$(masterpid "$bpid")（原 $b0）链接 $(readlink "$R/sbin/syapp")"
  out=$("${as[@]}" "$R/bin/syapp-wrapper" --instance blue upgrade-start $UP 2>&1) || bad "blue 重试 upgrade-start：$out"
  "${as[@]}" "$R/bin/syapp-wrapper" --instance blue upgrade-commit >/dev/null 2>&1 || bad "blue upgrade-commit"
  sleep 1
  n=$(load_stop user)
  [[ "$(exe "$dpid")" == "$newbin" && "$(exe "$bpid")" == "$newbin" ]] && ok "MI-03 两个实例都运行新版本" || bad "MI-03 运行版本：$(exe "$dpid") / $(exe "$bpid")"
  [[ "$n" == 0 ]] && ok "MI-03 热升级期间持续请求零错误" || bad "MI-03 热升级期间 $n 次请求失败：$(head -3 "$W/load-user.err" | tr '\n' ' ')"

  # ensure（cron 每分钟执行）：热升级后的 master 运行版本库中的二进制，仍须识别为在运行，不能再启动一份
  local m0 m1; m0="$(masterpid "$dpid") $(masterpid "$bpid")"
  out=$("${as[@]}" "$R/bin/syapp-instance" --root "$R" ensure 2>&1)
  m1="$(masterpid "$dpid") $(masterpid "$bpid")"
  [[ "$m0" == "$m1" && "$out" != *已启动数据面* ]] && ok "热升级后 ensure 不重复启动数据面" || bad "热升级后 ensure 误判：$out（master $m0 → $m1）"

  echo "== 删除 blue（默认实例持续请求）"
  load_start userrm 18181
  out=$("${as[@]}" "$R/bin/syapp-instance" --root "$R" remove blue --yes 2>&1) || bad "remove blue：$out"
  n=$(load_stop userrm)
  [[ ! -d "$R/instances/blue" && -d "$R/removed-logs" ]] && ok "blue 已删除，日志保留在 removed-logs" || bad "blue 删除结果不对"
  sleep 1
  pgrep -u "$U" -f "$R/instances/blue/" >/dev/null && bad "删除后 blue 的（已热升级的）master 仍在运行" || ok "删除后 blue 的 master 已退出"
  [[ "$n" == 0 ]] && ok "删除 blue 时默认实例零错误" || bad "删除 blue 时默认实例 $n 次失败"
  # 清理：让默认实例退出
  "${as[@]}" "$R/bin/syapp-wrapper" quit >/dev/null 2>&1
  pkill -u "$U" -f "$R/bin/syapp-agent" 2>/dev/null || true
}

system_phase() {
  echo "== system 模式（MI-01 / MI-02 / MI-03 / MI-04 / MI-05 / MI-07；D5 运行时不使用 root）"
  command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || { bad "system 阶段需要 systemd"; return; }
  systemctl stop syapp.service 'syapp@*' >/dev/null 2>&1 || true
  rm -rf /opt/syapp /var/lib/syapp-agent* /var/log/syapp* /var/cache/syapp*
  OUT="$W/pkg" scripts/release/package.sh node "$ARCH" "$TGZ" >/dev/null || { bad "打节点包失败"; return; }
  rm -rf "$W/nd" && mkdir -p "$W/nd" && tar -C "$W/nd" -xzf "$(ls "$W"/pkg/edgetraffic-os-node-[0-9]*-linux-"$ARCH".tar.gz)"
  local P; P=$(ls -d "$W"/nd/edgetraffic-os-node-[0-9]*/ | head -1)
  out=$(cd "$P" && ./install.sh 2>&1) && ok "安装节点包（默认实例）" || { bad "install.sh：$out"; return; }
  install -m 0755 "$W/wrapper" /usr/libexec/syapp/syapp-wrapper   # 注入 CI 公钥（升级包验签）
  syapp-instance nonroot >/dev/null 2>&1   # 替换文件后重新设置 wrapper 的属组、权限与 CAP_NET_BIND_SERVICE
  # security.capability：VFS_CAP_REVISION_2，CAP_NET_BIND_SERVICE（bit 10）在 permitted 与 inheritable，不设 effective
  capx=$(python3 -c "import os,sys;print(os.getxattr(sys.argv[1],'security.capability').hex())" /usr/libexec/syapp/syapp-wrapper 2>&1)
  [[ "$capx" == 000000020004000000040000* && "$(stat -c '%a %U %G' /usr/libexec/syapp/syapp-wrapper)" == "750 root syapp" ]] \
    && ok "wrapper 只带 CAP_NET_BIND_SERVICE（不是 setuid），只允许 syapp 组执行" || bad "wrapper 文件能力 $capx / $(stat -c '%a %U %G' /usr/libexec/syapp/syapp-wrapper)"

  echo "== D5 运行时不使用 root：升级前以 root 运行的 master 无中断切换"
  # 模拟升级前的节点：单元以 root 运行（旧版本的单元），master 以 root 启动
  mkdir -p /etc/systemd/system/syapp.service.d
  # 旧单元以 root 运行（不限制能力）。CapabilityBoundingSet= 置空会清空边界集（连 root 也无法绑定 80），
  # 因此清空后重新列出旧版 root master 实际用到的能力
  printf '[Service]\nUser=\nGroup=\nAmbientCapabilities=\nCapabilityBoundingSet=\nCapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_KILL CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_NET_BIND_SERVICE CAP_SYS_RESOURCE\nNoNewPrivileges=no\n' > /etc/systemd/system/syapp.service.d/legacy-root.conf
  systemctl daemon-reload && systemctl restart syapp.service
  local LD=/run/syapp/syapp.pid
  [[ "$(uid_of "$(masterpid $LD)")" == 0 ]] && ok "模拟升级前：master 以 root 运行" || bad "模拟升级前的 master 不是 root"
  rm -f /etc/systemd/system/syapp.service.d/legacy-root.conf && systemctl daemon-reload   # 新版本的单元
  local m0; m0=$(masterpid $LD)
  load_start legacy 8081
  out=$(syapp-instance nonroot 2>&1) || bad "nonroot：$out"
  sleep 1
  n=$(load_stop legacy)
  local m1; m1=$(masterpid $LD)
  [[ "$m1" != "$m0" && "$(uid_of "$m1")" == "$(id -u syapp)" ]] && ok "master 已无中断切换为 syapp 运行（$m0 → $m1）" || bad "切换后 master $m1 uid $(uid_of "$m1")：$out"
  [[ "$(awk '/^CapEff:/{print $2}' /proc/$m1/status)" == 0000000000000400 ]] && ok "新 master 只有 CAP_NET_BIND_SERVICE" || bad "新 master 能力 $(awk '/^CapEff:/{print $2}' /proc/$m1/status)"
  [[ "$n" == 0 ]] && ok "切换期间持续请求零错误" || bad "切换期间 $n 次请求失败"
  systemctl reload syapp.service && systemctl is-active -q syapp.service && ok "切换后 systemctl reload 正常（单元以 syapp 执行）" || bad "切换后 reload 失败"
  out=$(/usr/libexec/syapp/syapp-wrapper reload 2>&1) && bad "wrapper 以 root 运行未被拒绝" || ok "wrapper 拒绝以 root 运行"

  echo "== D5 防火墙以专用用户运行（只带 CAP_NET_ADMIN）"
  out=$(env -u SSH_CONNECTION syapp-firewall enable --ssh-from 10.0.0.0/8 2>&1) && ok "启用节点防火墙" || bad "启用防火墙：$out"
  [[ "$(stat -c %U /var/lib/syapp-firewall/firewall.nft 2>/dev/null)" == syapp-firewall ]] && ok "规则文件属于 syapp-firewall" || bad "规则文件属主 $(stat -c %U /var/lib/syapp-firewall/firewall.nft 2>&1)"
  for spec in "blue 18080 18443 8082" "green 28080 28443 8083"; do
    set -- $spec
    out=$(syapp-instance add "$1" --http "$2" --https "$3" --status "$4" 2>&1) && ok "添加实例 $1" || bad "添加 $1：$out"
  done
  local D=/run/syapp/syapp.pid B=/run/syapp-blue/syapp.pid G=/run/syapp-green/syapp.pid
  # 实例增删后由刷新服务（防火墙用户）按实例配置放行
  nft list table inet syapp_fw 2>/dev/null | grep -q 28080 && ok "新增实例 green 后防火墙已放行 28080（刷新服务以非 root 运行）" \
    || bad "防火墙未放行 28080：$(journalctl -u syapp-firewall-refresh.service --no-pager -n 5 2>&1 | tr '\n' ' ')"
  [[ "$(systemctl show -p User --value syapp-firewall-refresh.service)" == syapp-firewall ]] && ok "刷新服务以 syapp-firewall 运行" || bad "刷新服务身份不对"
  # 运行时没有 root 进程：全部实例的 master 与 worker
  local roots; roots=$(ps -C syapp -o user= | grep -cx root)
  [[ "$roots" == 0 ]] && ok "D5 三个实例的 master 与 worker 都不以 root 运行" || bad "D5 有 $roots 个数据面进程以 root 运行"

  echo "== MI-02 端口冲突"
  out=$(syapp-instance add red --http 18080 --https 38443 --status 8084 2>&1) && bad "与 blue 端口冲突未被拒绝" \
    || { [[ "$out" == *18080* ]] && ok "与 blue 的端口冲突被拒绝并指出端口" || bad "冲突信息不明：$out"; }
  python3 -m http.server -b 0.0.0.0 38080 >/dev/null 2>&1 & local hp=$!; sleep 1
  out=$(syapp-instance add red --http 38080 --https 38443 --status 8084 2>&1) && bad "被其他程序占用的端口未被拒绝" \
    || ok "被其他程序占用的端口被拒绝"
  kill $hp 2>/dev/null
  out=$(syapp-instance check 2>&1) && ok "syapp-instance check 未发现问题" || bad "check：$out"

  echo "== MI-01 单实例 reload 只替换该实例的 worker"
  local dm bm gm dw bw gw
  dm=$(masterpid $D); bm=$(masterpid $B); gm=$(masterpid $G); dw=$(workers $D); bw=$(workers $B); gw=$(workers $G)
  runuser -u syapp-agent-blue -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper --instance blue reload >/dev/null 2>&1 || bad "blue 的 agent 用户 reload 失败"
  sleep 1.5
  [[ "$(workers $B)" != "$bw" && "$(masterpid $B)" == "$bm" ]] && ok "blue worker 已替换，master 不变" || bad "blue reload 结果不对"
  [[ "$(masterpid $D)" == "$dm" && "$(workers $D)" == "$dw" && "$(masterpid $G)" == "$gm" && "$(workers $G)" == "$gw" ]] \
    && ok "默认实例与 green 的 master / worker 都不变" || bad "reload blue 影响了其他实例"

  echo "== MI-05 跨实例操作被 sudo 拒绝"
  runuser -u syapp-agent-blue -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper --instance green reload >/dev/null 2>&1 \
    && bad "syapp-agent-blue 能操作 green" || ok "syapp-agent-blue 操作 green 被拒绝"
  runuser -u syapp-agent-blue -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper reload >/dev/null 2>&1 \
    && bad "syapp-agent-blue 能操作默认实例" || ok "syapp-agent-blue 操作默认实例被拒绝"

  echo "== MI-07 reopen-all"
  for f in /var/log/syapp/error.log /var/log/syapp-blue/error.log /var/log/syapp-green/error.log; do mv "$f" "$f.1" 2>/dev/null; done
  syapp-instance reopen-all >/dev/null 2>&1; sleep 1
  for f in /var/log/syapp/error.log /var/log/syapp-blue/error.log /var/log/syapp-green/error.log; do
    [[ -f "$f" ]] && ok "重新打开 $f" || bad "reopen-all 后没有 $f"
  done

  echo "== MI-03 三个实例的主机级热升级，持续请求；blue 注入失败后重试"
  for a in /var/lib/syapp-agent /var/lib/syapp-agent-blue /var/lib/syapp-agent-green; do
    mkdir -p "$a/upgrade"; drop "$a/upgrade"
  done
  local newbin=/opt/syapp/versions/$UP/sbin/syapp
  local W_D=(runuser -u syapp-agent -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper)
  local W_B=(runuser -u syapp-agent-blue -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper --instance blue)
  local W_G=(runuser -u syapp-agent-green -- sudo -n -u syapp /usr/libexec/syapp/syapp-wrapper --instance green)
  load_start sys 8081 8082 8083
  out=$("${W_D[@]}" upgrade-start $UP 2>&1) || bad "默认实例 upgrade-start：$out"
  "${W_D[@]}" upgrade-commit >/dev/null 2>&1 || bad "默认实例 upgrade-commit"
  bm=$(masterpid $B)
  out=$("${W_B[@]}" upgrade-start $UP 2>&1) || bad "blue upgrade-start：$out"
  "${W_B[@]}" upgrade-abort >/dev/null 2>&1 || bad "blue upgrade-abort"
  [[ "$(masterpid $B)" == "$bm" && "$(exe $B)" != "$newbin" && "$(readlink /opt/syapp/sbin/syapp)" == "$newbin" ]] \
    && ok "blue 注入失败：回到原 master（旧版本），共用软链保持新版本，主机部分升级" || bad "blue 中止结果不对：exe=$(exe $B)"
  out=$("${W_B[@]}" upgrade-start $UP 2>&1) || bad "blue 重试：$out"
  "${W_B[@]}" upgrade-commit >/dev/null 2>&1 || bad "blue upgrade-commit"
  out=$("${W_G[@]}" upgrade-start $UP 2>&1) || bad "green upgrade-start：$out"
  "${W_G[@]}" upgrade-commit >/dev/null 2>&1 || bad "green upgrade-commit"
  sleep 1
  n=$(load_stop sys)
  [[ "$(exe $D)" == "$newbin" && "$(exe $B)" == "$newbin" && "$(exe $G)" == "$newbin" ]] \
    && ok "MI-03 三个实例都运行新版本" || bad "MI-03 运行版本：$(exe $D) / $(exe $B) / $(exe $G)"
  [[ "$n" == 0 ]] && ok "MI-03 热升级期间持续请求零错误" || bad "MI-03 $n 次请求失败：$(head -3 "$W/load-sys.err" | tr '\n' ' ')"
  systemctl is-active -q syapp.service syapp@blue.service syapp@green.service && ok "全程没有实例停止" || bad "有实例的单元不在运行"

  echo "== MI-04 删除 blue（默认实例与 green 持续请求）；有具名实例时卸载被拒绝"
  load_start rm 8081 8083
  out=$(syapp-instance remove blue --yes 2>&1) || bad "remove blue：$out"
  n=$(load_stop rm)
  [[ "$n" == 0 ]] && ok "MI-04 删除 blue 期间其他实例零错误" || bad "MI-04 $n 次请求失败"
  id syapp-agent-blue >/dev/null 2>&1 && bad "blue 的 agent 用户未删除" || ok "blue 的 agent 用户与 sudoers 已删除"
  out=$(cd "$P" && ./uninstall.sh 2>&1) && bad "有具名实例时卸载未被拒绝" || { [[ "$out" == *green* ]] && ok "MI-04 有具名实例（green）时卸载被拒绝" || bad "卸载拒绝信息不明：$out"; }
  systemctl is-active -q syapp@green.service && ok "被拒绝的卸载没有影响 green" || bad "green 被停止"
  syapp-firewall disable >/dev/null 2>&1 || true
}

case "$PHASE" in
  user) user_phase ;;
  system) system_phase ;;
  all) user_phase; system_phase ;;
  *) echo "phase: user|system|all" >&2; exit 2 ;;
esac
rm -rf "$W"
echo "---- $fail failed"
[[ $fail -eq 0 ]] && echo "P11 ACCEPTANCE OK" || exit 1
