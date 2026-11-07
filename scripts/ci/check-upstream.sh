#!/usr/bin/env bash
# 检查数据面锁定的上游源码与静态依赖是否有新版本（docs/upstream-upgrade.md 第 2.3 节）。
# 用法：scripts/ci/check-upstream.sh
# 由 .github/workflows/upstream-check.yml 每周运行；同一版本线有新版本时以退出码 1 提醒，查询失败时退出码 2。
#
# 只在当前锁定的版本线内比较（nginx 1.30.x、OpenSSL 3.5.x、铜锁 8.x）：换版本线是功能升级，需单独评估（第 4 节）。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/../.."

# shellcheck disable=SC1091
source "$ROOT/dataplane/UPSTREAM"
# shellcheck disable=SC1091
source "$ROOT/dataplane/DEPS"

rc=0
note() { [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::warning title=$1::$2"; echo "🔴 $1：$2"; }

# tags <仓库> <tag 模式> <去掉的前缀>：列出发布标签对应的版本号（--refs 不输出带 ^{} 的附注标签条目），只保留纯数字版本
tags() {
  git ls-remote --tags --refs "$1" "$2" 2>/dev/null | awk -F/ '{print $NF}' | sed "s/^$3//" \
    | grep -E '^[0-9]+(\.[0-9]+)+$' | sort -V || true
}

# check <名称> <当前版本> <同一版本线内的全部版本>
check() {
  local name=$1 cur=$2 all=$3 latest
  latest=$(tail -1 <<<"$all")
  if [[ -z "$latest" ]]; then
    echo "⚠  $name：无法获取远程标签"
    [[ $rc == 0 ]] && rc=2
  elif [[ "$(printf '%s\n%s\n' "$cur" "$latest" | sort -V | tail -1)" != "$cur" ]]; then
    note "$name 新版本可用" "$latest（当前 $cur）"
    rc=1
  else
    echo "✅ $name $cur 已是最新"
  fi
}

# --- 上游源码（只跟 stable：次版本号为偶数） ---
series="${UPSTREAM_VERSION%.*}"
all=$(tags "$UPSTREAM_GIT" 'release-*' 'release-')
check "上游 stable $series" "$UPSTREAM_VERSION" "$(grep -E "^${series//./\\.}\.[0-9]+$" <<<"$all" || true)"
newer=$(awk -F. -v M="${series%.*}" -v m="${series#*.}" '$1==M && $2>m && $2%2==0 {print $1"."$2}' <<<"$all" | sort -uV | tail -1)
[[ -n "$newer" ]] && echo "ℹ  已有更新的 stable 版本线 $newer（大版本升级需单独评估，见 docs/upstream-upgrade.md 第 4 节）"

# --- OpenSSL（同一版本线，如 3.5.x） ---
s="${OPENSSL_VERSION%.*}"
check "OpenSSL $s" "$OPENSSL_VERSION" "$(tags https://github.com/openssl/openssl.git "openssl-${s}.*" 'openssl-')"

# --- PCRE2 ---
check "PCRE2" "$PCRE2_VERSION" "$(tags https://github.com/PCRE2Project/pcre2.git 'pcre2-*' 'pcre2-')"

# --- zlib ---
check "zlib" "$ZLIB_VERSION" "$(tags https://github.com/madler/zlib.git 'v*' 'v')"

# --- 铜锁（国密变体，同一主版本） ---
check "铜锁 ${TONGSUO_VERSION%%.*}.x" "$TONGSUO_VERSION" "$(tags "$TONGSUO_GIT" "${TONGSUO_VERSION%%.*}.*" '')"

exit $rc
