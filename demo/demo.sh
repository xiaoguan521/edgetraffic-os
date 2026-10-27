#!/usr/bin/env bash
# EdgeTraffic OS 演示助手。在 demo/ 目录下运行。
#
#   ./demo.sh up                 构建并启动演示环境（首次约 5–10 分钟）
#   ./demo.sh license [天数]     用演示密钥签发授权，输出 out/demo.lic（默认 365 天）
#   ./demo.sh license-expired    签发一份已过期的授权，用于演示“过期只读”
#   ./demo.sh enroll <node> <控制台“添加节点”给出的命令>
#                                在节点容器里执行注册命令，例如：
#                                ./demo.sh enroll node1 sudo syapp-agent enroll -server ... -token ... -ca-fingerprint ...
#   ./demo.sh traffic [秒数]     向两台节点持续发请求，让总览页出现流量曲线（默认 300 秒）
#   ./demo.sh ps <node>          查看节点上的进程（展示进程名为 syapp）
#   ./demo.sh status             查看容器状态与访问地址
#   ./demo.sh down               停止（保留数据）
#   ./demo.sh reset              停止并清空全部数据，回到初始状态
set -euo pipefail
cd "$(dirname "$0")"
DC="docker compose"
KEY=keys/etos-demo-2026.key

host_ip() { hostname -I 2>/dev/null | awk '{print $1}' || echo 127.0.0.1; }

issue() {
  [[ -f "$KEY" ]] || { echo "缺少演示签名私钥 $KEY（请向项目负责人索取，放到 demo/keys/ 下，不要提交到仓库）" >&2; exit 1; }
  local inst
  inst=$($DC exec -T console sh -c 'cat /var/lib/syapp-console/installation.json' | sed -E 's/.*"installation_id":"([^"]+)".*/\1/')
  [[ "$inst" == inst_* ]] || { echo "读取控制台安装 ID 失败，控制台是否已启动？" >&2; exit 1; }
  mkdir -p out
  $DC exec -T console syapp-license-server issue -key /keys/etos-demo-2026.key -kid etos-demo-2026 \
    -installation-id "$inst" -binding installation_only -customer "${CUSTOMER:-演示客户}" \
    -edition "${EDITION:-professional}" -nodes "${NODES:-10}" -sites "${SITES:-200}" "$@" > out/"$OUT"
  echo "已签发：demo/out/$OUT（安装 ID $inst）"
  echo "在控制台“授权与系统”页选择该文件导入。"
}

case "${1:-}" in
  up)
    mkdir -p keys out
    $DC up -d --build
    echo
    echo "控制台：  http://$(host_ip):${CONSOLE_PORT:-8443}"
    echo "节点 1：  http://$(host_ip):${NODE1_PORT:-8001}   节点 2：http://$(host_ip):${NODE2_PORT:-8002}"
    ;;
  license)
    OUT=demo.lic issue -days "${2:-365}"
    ;;
  license-expired)
    OUT=expired.lic issue -expires "$(date -d '-20 days' +%F)" -grace 15
    ;;
  enroll)
    node="${2:?usage: ./demo.sh enroll <node1|node2> <command>}"
    shift 2
    [[ "${1:-}" == "sudo" ]] && shift
    cmd="$*"
    cmd="${cmd%% && *}"   # 去掉末尾的 systemctl 部分，容器入口脚本会自动启动 agent
    # shellcheck disable=SC2086
    $DC exec -T "$node" $cmd
    echo "$node 已注册，agent 将在几秒内连上控制台。"
    ;;
  traffic)
    secs="${2:-300}"
    host="${SITE_HOST:-shop.demo.local}"
    echo "向 node1 / node2 发送请求（Host: $host），持续 $secs 秒，Ctrl-C 结束"
    end=$((SECONDS + secs))
    while ((SECONDS < end)); do
      for p in "${NODE1_PORT:-8001}" "${NODE2_PORT:-8002}"; do
        curl -s -o /dev/null -H "Host: $host" "http://127.0.0.1:$p/" || true
        curl -s -o /dev/null -H "Host: $host" "http://127.0.0.1:$p/api/orders" || true
      done
      sleep 0.2
    done
    ;;
  ps)
    $DC exec "${2:?usage: ./demo.sh ps <node1|node2>}" sh -c 'syapp -v; echo; ps -eo user,pid,comm,args | grep -E "syapp|COMMAND" | grep -v grep'
    ;;
  status)
    $DC ps
    echo
    echo "控制台：  http://$(host_ip):${CONSOLE_PORT:-8443}"
    ;;
  down)
    $DC down
    ;;
  reset)
    $DC down -v
    rm -rf out
    echo "演示数据已清空。"
    ;;
  *)
    sed -n '2,17p' "$0"
    exit 2
    ;;
esac
