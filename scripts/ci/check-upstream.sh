#!/usr/bin/env bash
# 检查 nginx stable 和静态依赖是否有新版本。
# 用法：scripts/ci/check-upstream.sh
# 建议每周在 CI 中定时运行；有新版本时以非零退出码提醒。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/../.."

# shellcheck disable=SC1091
source "$ROOT/dataplane/UPSTREAM"
# shellcheck disable=SC1091
source "$ROOT/dataplane/DEPS"

rc=0

# --- nginx stable ---
major_minor="${UPSTREAM_VERSION%.*}"
latest_nginx=$(git ls-remote --tags https://github.com/nginx/nginx.git "release-${major_minor}.*" 2>/dev/null \
  | awk -F/ '{print $NF}' | sed 's/release-//' | sort -V | tail -1)

if [[ -z "$latest_nginx" ]]; then
  echo "⚠  无法获取 nginx 远程 tag"
elif [[ "$latest_nginx" != "$UPSTREAM_VERSION" ]]; then
  echo "🔴 nginx 新版本可用: $latest_nginx（当前 $UPSTREAM_VERSION）"
  echo "   发布说明: https://nginx.org/en/CHANGES-${major_minor}"
  rc=1
else
  echo "✅ nginx stable $UPSTREAM_VERSION 已是最新"
fi

# --- OpenSSL ---
latest_openssl=$(git ls-remote --tags https://github.com/openssl/openssl.git "openssl-${OPENSSL_VERSION%%.*}.*" 2>/dev/null \
  | awk -F/ '{print $NF}' | sed 's/openssl-//' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)

if [[ -n "$latest_openssl" && "$latest_openssl" != "$OPENSSL_VERSION" ]]; then
  echo "🔴 OpenSSL 新版本可用: $latest_openssl（当前 $OPENSSL_VERSION）"
  rc=1
else
  echo "✅ OpenSSL $OPENSSL_VERSION 已是最新"
fi

# --- PCRE2 ---
latest_pcre2=$(git ls-remote --tags https://github.com/PCRE2Project/pcre2.git 'pcre2-*' 2>/dev/null \
  | awk -F/ '{print $NF}' | sed 's/pcre2-//' | grep -E '^[0-9]+\.[0-9]+$' | sort -V | tail -1)

if [[ -n "$latest_pcre2" && "$latest_pcre2" != "$PCRE2_VERSION" ]]; then
  echo "🔴 PCRE2 新版本可用: $latest_pcre2（当前 $PCRE2_VERSION）"
  rc=1
else
  echo "✅ PCRE2 $PCRE2_VERSION 已是最新"
fi

exit $rc
