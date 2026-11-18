#!/usr/bin/env bash
# 静态校验“兼容配置接入 SkyWalking”的示例（demo/skywalking-legacy-example），不需要数据面二进制，也不需要 root。
#   用法：scripts/ci/skywalking-conf-check.sh [示例目录] [skywalking 库 zip]
#   覆盖这次线上故障的几类根因：初始化缺失、库缺失、location 里的 Lua 块不成对、游离的 log_by_lua_block、
#   同级别重复指令。真实的 syedge -t 与 reload 仍要在节点上做（见 LUA-PLACEMENT.md 第 4 节）。
set -uo pipefail
cd "$(dirname "$0")/../.."
DIR=${1:-demo/skywalking-legacy-example}
ZIP=${2:-demo/skywalking-nginx-lua-0.5.0.zip}
MAIN="$DIR/syedge-443.conf"
INIT="$DIR/conf.d/10-skywalking.conf"

fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; [[ -z "${GITHUB_ACTIONS:-}" ]] || echo "::error title=skywalking-conf-check::$1"; fail=$((fail+1)); }

for f in "$MAIN" "$INIT" "$ZIP"; do
  [[ -f "$f" ]] || { bad "缺少文件 $f"; echo "---- $fail failed"; exit 1; }
done

# 去掉整行注释后的有效内容
active() { grep -vE '^[[:space:]]*#' "$1"; }
count()  { active "$1" | grep -cE "$2" || true; }

echo "== 1. 主配置结构"
o=$(active "$MAIN" | tr -cd '{' | wc -c | tr -d " "); c=$(active "$MAIN" | tr -cd '}' | wc -c | tr -d " ")
[[ "$o" == "$c" ]] && ok "大括号配平（$o 对）" || bad "大括号不配平：{=$o }=$c"
[[ $(count "$MAIN" 'include[[:space:]]+conf\.d/\*\.conf;') -ge 1 ]] && ok "主配置包含 conf.d/*.conf" || bad "主配置没有 include conf.d/*.conf，10-skywalking.conf 不会被加载"

echo "== 2. location 里的链路块成对"
start=$(count "$MAIN" 'skywalking_tracer:start')
fin=$(count "$MAIN" 'skywalking_tracer:finish')
rep=$(count "$MAIN" 'skywalking_tracer:prepareForReport')
rw=$(count "$MAIN" 'rewrite_by_lua_block')
bf=$(count "$MAIN" 'body_filter_by_lua_block')
lg=$(count "$MAIN" 'log_by_lua_block')
echo "     start=$start finish=$fin report=$rep | rewrite=$rw body_filter=$bf log=$lg"
[[ "$start" == "$fin" && "$fin" == "$rep" ]] && ok "start / finish / prepareForReport 数量一致" || bad "start/finish/prepareForReport 数量不一致（有 location 缺块）"
[[ "$rw" == "$bf" && "$bf" == "$lg" ]] && ok "rewrite / body_filter / log 三类块数量一致" || bad "三类 Lua 块数量不一致（可能有游离的 log_by_lua_block 或缺块）"
# 逐个 location 检查：同一个 location 里 start、finish、prepareForReport 必须都出现
unbalanced=$(active "$MAIN" | awk '
  /^[[:space:]]*location[[:space:]]/ { if (loc != "" && !(s == f && f == p)) print loc " start=" s " finish=" f " report=" p; loc = NR ": " $0; s = f = p = 0 }
  /skywalking_tracer:start/ { s++ } /skywalking_tracer:finish/ { f++ } /skywalking_tracer:prepareForReport/ { p++ }
  END { if (loc != "" && !(s == f && f == p)) print loc " start=" s " finish=" f " report=" p }')
[[ -z "$unbalanced" ]] && ok "每个 location 的三段调用齐全" || { bad "以下 location 的链路调用不齐全："; echo "$unbalanced" | sed 's/^/       /'; }

echo "== 3. 初始化（conf.d/10-skywalking.conf）"
need=(
  'lua_shared_dict[[:space:]]+tracing_buffer'
  'lua_shared_dict[[:space:]]+prometheus_metrics'
  'init_worker_by_lua_block'
  'skywalking_tracer[[:space:]]*=[[:space:]]*require\("skywalking\.tracer"\)'
  'startBackendTimer\(OAP_ADDRESS\)'
  'local[[:space:]]+OAP_ADDRESS[[:space:]]*=[[:space:]]*"http://[^"]+"'
  'local[[:space:]]+SERVICE_NAME[[:space:]]*='
  'local[[:space:]]+INSTANCE_NAME[[:space:]]*='
  'serviceName'
  'serviceInstanceName'
  '"nginx_http_size_bytes"'
  '"nginx_http_requests_total"'
  '"nginx_http_latency"'
  '"nginx_http_connections"'
  'metric_duration[[:space:]]*=[[:space:]]*prometheus:histogram'
  'metric_duration:observe'
  'prometheus[[:space:]]*=[[:space:]]*require\("prometheus"\)'
  'metric_connections[[:space:]]*='
)
for p in "${need[@]}"; do
  [[ $(count "$INIT" "$p") -ge 1 ]] && ok "含 $p" || bad "10-skywalking.conf 缺少：$p"
done
# 同一级别只能有一个：主配置里不能再有，否则 syedge -t 报 duplicate
for d in 'init_worker_by_lua_block' 'lua_shared_dict[[:space:]]+tracing_buffer' 'lua_shared_dict[[:space:]]+prometheus_metrics'; do
  n=$(( $(count "$MAIN" "$d") + $(count "$INIT" "$d") ))
  [[ "$n" == 1 ]] && ok "$d 只定义一次" || bad "$d 定义了 $n 次（同级别重复会让 syedge -t 失败）"
done
# 主配置里用到的全局变量必须由初始化创建
for v in metric_connections prometheus; do
  if [[ $(count "$MAIN" "${v}:") -ge 1 && $(count "$INIT" "${v}[[:space:]]*=") -lt 1 ]]; then
    bad "主配置使用了 $v，但初始化里没有创建"
  fi
done

echo "== 4. lua_package_path 与库"
lpp=$(active "$MAIN" | grep -E 'lua_package_path' | head -1)
[[ "$lpp" == *"/lualib/?.lua"* ]] && ok "lua_package_path 含 lualib" || bad "lua_package_path 不含 lualib 目录"
[[ $(count "$MAIN" 'lua_package_path') == 1 ]] && ok "lua_package_path 只出现一次" || bad "lua_package_path 出现次数不是 1"
# 搜索路径里有 R/lualib（安装根目录下的 lualib，而不是 modules/ 下的目录）
first=$(echo "$lpp" | sed -E 's/.*lua_package_path[[:space:]]+"([^;"]+).*/\1/')
[[ "$first" == */lualib/?.lua && "$first" != */modules/* ]] && ok "lua_package_path 以 R/lualib 开头" || bad "lua_package_path 第一项应是 R/lualib（当前：$first），自有库才不会随附加包升级丢失"

list=$(unzip -Z1 "$ZIP")
lib() { echo "$list" | grep -qxE "[^/]+/lib/$1"; }
for f in prometheus.lua prometheus_keys.lua prometheus_resty_counter.lua resty/http.lua resty/http_headers.lua resty/jit-uuid.lua skywalking/tracer.lua skywalking/client.lua skywalking/util.lua; do
  lib "$f" && ok "压缩包含 $f" || bad "压缩包缺少 $f"
done

echo "== 5. 库的依赖闭合（require 的模块要么在压缩包里，要么由 LuaJIT/附加包提供）"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
unzip -q "$ZIP" -d "$T" || { bad "无法解压 $ZIP"; echo "---- $fail failed"; exit 1; }
LIBDIR=$(find "$T" -maxdepth 2 -type d -name lib | head -1)
PROVIDED=' bit ffi cjson cjson.safe table.clear tablepool resty.core resty.lrucache '
missing=""
while read -r mod; do
  [[ -n "$mod" ]] || continue
  [[ "$PROVIDED" == *" $mod "* ]] && continue
  p=${mod//.//}
  [[ -f "$LIBDIR/$p.lua" || -f "$LIBDIR/$p/init.lua" ]] || missing="$missing $mod"
done < <(cd "$LIBDIR" && find skywalking prometheus.lua prometheus_keys.lua prometheus_resty_counter.lua resty -name '*.lua' ! -name '*_test.lua' -print0 \
  | xargs -0 grep -hoE "require[ (]+['\"][A-Za-z0-9_.-]+['\"]" | sed -E "s/.*['\"]([^'\"]+)['\"]/\1/" | sort -u)
[[ -z "$missing" ]] && ok "依赖全部可解析" || bad "有依赖既不在压缩包里，也不是附加包提供的：$missing"
# 配置里直接 require 的模块
while read -r mod; do
  [[ -n "$mod" ]] || continue
  p=${mod//.//}
  [[ -f "$LIBDIR/$p.lua" ]] && ok "配置 require(\"$mod\") 可在库中找到" || bad "配置 require(\"$mod\") 在库中找不到"
done < <(active "$INIT" | grep -oE 'require\("[A-Za-z0-9_.]+"\)' | sed -E 's/require\("([^"]+)"\)/\1/' | sort -u)

echo "== 6. 随示例提供的 lualib（运维整目录复制即可）"
LUALIB="$DIR/lualib"
[[ -d "$LUALIB" ]] || bad "缺少 $LUALIB"
# 与压缩包逐文件一致（不含测试文件）：防止被改动或遗漏
drift=""
while IFS= read -r f; do
  rel=${f#"$LIBDIR"/}
  [[ "$rel" == *_test.lua ]] && continue
  case "$rel" in skywalking/*|prometheus*.lua|resty/http.lua|resty/http_headers.lua|resty/jit-uuid.lua) ;; *) continue ;; esac
  cmp -s "$f" "$LUALIB/$rel" || drift="$drift $rel"
done < <(find "$LIBDIR" -type f -name '*.lua')
[[ -z "$drift" ]] && ok "lualib 与压缩包一致（缺失或改动的文件：无）" || bad "lualib 与压缩包不一致：$drift"
extra=$(cd "$LUALIB" && find . -type f -name '*.lua' | sed 's|^\./||' | while read -r r; do [[ -f "$LIBDIR/$r" ]] || echo "$r"; done)
[[ -z "$extra" ]] && ok "lualib 里没有压缩包以外的 Lua 文件" || bad "lualib 里有压缩包以外的 Lua 文件：$extra"
[[ -z "$(find "$LUALIB" -name '*_test.lua')" ]] && ok "lualib 不含测试文件" || bad "lualib 含测试文件"
for f in README.txt LICENSE-prometheus; do [[ -f "$LUALIB/$f" ]] && ok "lualib/$f 存在" || bad "lualib/$f 缺失"; done

echo "== 7. 示例必须脱敏"
if grep -rnE '(^|[^0-9])(10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|192\.168\.[0-9]+\.[0-9]+)([^0-9]|$)' "$DIR" --include='*.conf' --include='*.md' >/dev/null; then
  bad "示例里出现私网地址，请换成 192.0.2.x / example.com"
else
  ok "示例里没有私网地址"
fi

echo "---- $fail failed"
[[ $fail -eq 0 ]]
