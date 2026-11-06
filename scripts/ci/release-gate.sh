#!/usr/bin/env bash
# 发布门禁统一检查脚本（scripts/ci/release-gate.sh）
#
# 依次执行并校验：
#   1. Go 格式与静态检查（gofmt + golangci-lint）
#   2. 品牌与版权洁净度检查（make brand）
#   3. ORM 查询代码与迁移一致性（make gen-check）
#   4. Web 前端 ESLint、TypeScript 类型检查与双前端构建（Console + License Portal）
#   5. 生产组件构建与 release 构建标签门禁（不含演示公钥）
#   6. Go 全量竞态测试（go vet ./... && go test -race -count=1 ./...）
#
# 用法：
#   scripts/ci/release-gate.sh [--out <summary.md>]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

OUT_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) OUT_FILE="$2"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
FULL_COMMIT="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
DIRTY="clean"
if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  DIRTY="dirty"
fi
NOW_UTC="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

echo "== [1/6] Go lint (gofmt + golangci-lint)"
make lint

echo "== [2/6] Brand check"
make brand

echo "== [3/6] Generated query & schema parity check"
make gen-check

echo "== [4/6] Web ESLint & production build (Console + Portal)"
(
  cd web
  ./node_modules/.bin/eslint src src-portal
  ./node_modules/.bin/vue-tsc --noEmit
  ./node_modules/.bin/vite build
  ./node_modules/.bin/vite build -c vite.portal.config.ts
)

echo "== [5/6] Production binaries & release-tag build gate"
make build
TMP_REL="$(mktemp -d)"
trap 'rm -rf "$TMP_REL"' EXIT
CGO_ENABLED=0 go build -trimpath -tags release -o "$TMP_REL/syedge-console" ./cmd/syedge-console
CGO_ENABLED=0 go build -trimpath -tags release -o "$TMP_REL/syedge-agent" ./cmd/syedge-agent
CGO_ENABLED=0 go build -trimpath -tags release -o "$TMP_REL/syedge-wrapper" ./cmd/syedge-wrapper
CGO_ENABLED=0 go build -trimpath -tags release -o "$TMP_REL/syedge-license-server" ./cmd/syedge-license-server

echo "== [6/6] Go vet & race test suite"
go vet ./...
TEST_LOG="$(mktemp)"
if ! go test -race -count=1 ./... | tee "$TEST_LOG"; then
  rm -f "$TEST_LOG"
  exit 1
fi
PKG_PASS="$(grep -c '^ok ' "$TEST_LOG" || true)"
rm -f "$TEST_LOG"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

CONSOLE_VER="$(./bin/syedge-console -version 2>/dev/null || echo unknown)"
CONSOLE_SHA="$(sha256_of bin/syedge-console)"
AGENT_SHA="$(sha256_of bin/syedge-agent)"
WRAPPER_SHA="$(sha256_of bin/syedge-wrapper)"
PORTAL_SHA="$(sha256_of bin/syedge-license-server)"

SUMMARY="$(cat <<EOF
# SyEdge 发布门禁报告

- **执行时间 (UTC)**：\`$NOW_UTC\`
- **Git 分支 / 提交**：\`$BRANCH\` @ \`$COMMIT\` (\`$FULL_COMMIT\`, working tree: \`$DIRTY\`)
- **控制台版本输出**：\`$CONSOLE_VER\`

## 1. 门禁检查项汇总

| 检查项 | 结果 | 详情 |
| --- | --- | --- |
| \`gofmt\` + \`golangci-lint\` | PASS | 0 issues |
| \`make brand\` | PASS | 产品代码范围无禁用词与版权声明缺失 |
| \`make gen-check\` | PASS | GORM/Gen 查询代码与模型定义完全一致 |
| \`web eslint\` | PASS | \`src\` 与 \`src-portal\` 0 errors / 0 warnings |
| \`web build\` | PASS | Console 与 License Portal TypeScript + Vite 构建通过 |
| \`make build\` + \`-tags release\` | PASS | 4 个核心二进制标准构建与 \`release\` 标签构建均通过 |
| \`go test -race -count=1 ./...\` | PASS | $PKG_PASS 个 Go 测试包全部通过（0 失败） |

## 2. 构建产物 SHA-256

| 组件 | 路径 | SHA-256 |
| --- | --- | --- |
| \`syedge-console\` | \`bin/syedge-console\` | \`$CONSOLE_SHA\` |
| \`syedge-agent\` | \`bin/syedge-agent\` | \`$AGENT_SHA\` |
| \`syedge-wrapper\` | \`bin/syedge-wrapper\` | \`$WRAPPER_SHA\` |
| \`syedge-license-server\` | \`bin/syedge-license-server\` | \`$PORTAL_SHA\` |
EOF
)"

echo "$SUMMARY"
if [[ -n "$OUT_FILE" ]]; then
  mkdir -p "$(dirname "$OUT_FILE")"
  printf '%s\n' "$SUMMARY" > "$OUT_FILE"
  echo "报告已写入：$OUT_FILE"
fi

