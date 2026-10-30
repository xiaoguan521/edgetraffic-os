#!/usr/bin/env bash
# 品牌门禁（规格书 11）：产品代码、前端文案、打包与部署脚本中不得出现 nginx 字样。
# 白名单：关于页 / NOTICE 中的标准归因句 “based on NGINX Open Source (BSD-2-Clause)”（BSD 许可要求保留）。
# 不检查：dataplane/patches、dataplane/build-syapp*.sh、dataplane/UPSTREAM、dataplane/lua/DEPS（第三方仓库名）、docs/、README/CLAUDE/AGENTS、第三方依赖名（gonginx）。
set -euo pipefail
cd "$(dirname "$0")/.."
SCOPE=(cmd internal web/src web/index.html license-portal/src deploy dataplane/packaging api/proto dataplane/lua/install.sh dataplane/lua/uninstall.sh dataplane/lua/lualib)
hits=$(grep -rniI 'nginx' "${SCOPE[@]}" 2>/dev/null \
  | grep -v -i 'gonginx' \
  | grep -v 'NGINX Open Source (BSD-2-Clause)' \
  | grep -v '^internal/render/testdata/' || true)
if [[ -n "$hits" ]]; then
  echo "brand-check FAILED: 'nginx' found in product scope:"
  echo "$hits"
  exit 1
fi
echo "brand-check OK"
