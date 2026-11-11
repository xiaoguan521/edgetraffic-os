#!/usr/bin/env bash
# 文档门禁（运维体验手册 T1）：
#   1. README.md 与 docs/**/*.md 中的相对 Markdown 链接目标必须存在；
#   2. scripts/ci/docs-required.txt 中列出的 “文件<TAB>固定字符串” 必须出现在对应文件中。
# 只依赖 grep / sed / awk / find，不联网。
set -uo pipefail
cd "$(dirname "$0")/../.."

REQUIRED=scripts/ci/docs-required.txt
fails=0

fail() {
  local file="$1" reason="$2"
  echo "FAIL ${file}: ${reason}"
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::error file=${file}::${reason}"
  fi
  fails=$((fails + 1))
}

# 输出 “行号<TAB>链接目标”。跳过代码块（三个反引号之间）与行内代码（单个反引号之间）。
extract_links() {
  awk '
    /^[ \t]*```/ { infence = !infence; next }
    infence { next }
    {
      line = $0
      gsub(/`[^`]*`/, "", line)
      while (match(line, /\]\([^)]*\)/)) {
        t = substr(line, RSTART + 2, RLENGTH - 3)
        print NR "\t" t
        line = substr(line, RSTART + RLENGTH)
      }
    }
  ' "$1"
}

check_links() {
  local md="$1" dir lineno t path
  dir="$(dirname "$md")"
  while IFS=$'\t' read -r lineno t; do
    # 去掉可选的 “title” 部分与尖括号
    t="${t%% *}"
    t="${t#<}"
    t="${t%>}"
    case "$t" in
      ''|'#'*|http://*|https://*|mailto:*) continue ;;
    esac
    path="${t%%#*}"
    [[ -z "$path" ]] && continue
    if [[ ! -e "$dir/$path" ]]; then
      fail "$md" "line ${lineno}: broken relative link -> ${t}"
    fi
  done < <(extract_links "$md")
}

# 1. 相对链接
while IFS= read -r -d '' md; do
  check_links "$md"
done < <({ printf '%s\0' README.md; find docs -type f -name '*.md' -print0; })

# 2. 必需内容
if [[ ! -f "$REQUIRED" ]]; then
  fail "$REQUIRED" "required-content list is missing"
else
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      ''|'#'*) continue ;;
    esac
    if [[ "$line" != *$'\t'* ]]; then
      fail "$REQUIRED" "malformed line (expected <file><TAB><string>): ${line}"
      continue
    fi
    f="${line%%$'\t'*}"
    s="${line#*$'\t'}"
    if [[ ! -f "$f" ]]; then
      fail "$f" "required file does not exist"
    elif ! grep -F -q -- "$s" "$f"; then
      fail "$f" "required string not found: ${s}"
    fi
  done < "$REQUIRED"
fi

if (( fails > 0 )); then
  echo "docs-check FAILED: ${fails} problem(s)"
  exit 1
fi
echo "docs-check OK"
