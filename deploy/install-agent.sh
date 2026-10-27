#!/usr/bin/env bash
# 边缘节点一键安装（规格书 5.2 / 9.4）。控制台“添加节点”页面会生成带参数的完整命令。
# 用法：install-agent.sh --server console.internal:9443 --token <一次性注册令牌>
set -euo pipefail
SERVER=""; TOKEN=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --token)  TOKEN="$2";  shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$SERVER" && -n "$TOKEN" ]] || { echo "need --server and --token" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }

# TODO(P2)：
# 1. 安装 syapp 与 syapp-agent 软件包（RPM/DEB），创建用户 syapp、syapp-agent
# 2. 安装 /usr/libexec/syapp/syapp-wrapper（root:root 0755）与 /etc/sudoers.d/syapp-agent（0440，visudo -c 校验）
# 3. 初始化 /opt/syapp/conf/{releases,current,previous}
# 4. 写入 /etc/syapp-agent/agent.env（SYAPP_CONSOLE=$SERVER），执行 syapp-agent -server "$SERVER" -enroll-token "$TOKEN" 完成注册
# 5. 加载 nftables 规则，启用 syapp.service 与 syapp-agent.service
echo "install-agent.sh: not implemented yet (planned for P2)" >&2
exit 1
