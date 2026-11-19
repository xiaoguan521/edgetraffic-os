#!/usr/bin/env bash
# 检查整合包（syedge-all）的目录结构与 install.sh 的参数边界：不需要 root、systemd 或真实数据面。
#   整合包 = 节点包 + extras/gmssl（国密版二进制）+ extras/lua（Lua 附加包）。
#   真实安装由 release 工作流在正式标签上的整合包冒烟步骤覆盖。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
PREFIX=${SYEDGE_PREFIX:-/opt/syedge}
[[ "$PREFIX" =~ ^(/[A-Za-z0-9._@+-]+)+$ ]] || { echo "SYEDGE_PREFIX must be a clean absolute path" >&2; exit 2; }
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fail=$((fail+1)); }

# 假的节点包：只放整合包打包与 install.sh 参数检查需要的文件；install.sh 用真实模板（带编译前缀替换）
VER=9.9.9
N="$W/node/syedge-node-$VER-linux-amd64"
mkdir -p "$N/rootfs$PREFIX/sbin" "$W/out"
printf 'standard binary SyEdge/1.30.6\n' > "$N/rootfs$PREFIX/sbin/syedge"
sed "s|/opt/syedge|$PREFIX|g" deploy/node/install.sh > "$N/install.sh"
sed "s|/opt/syedge|$PREFIX|g" deploy/node/uninstall.sh > "$N/uninstall.sh"
chmod 0755 "$N/install.sh" "$N/uninstall.sh"
cp docs/NOTICE "$N/NOTICE"
tar -C "$W/node" -czf "$W/node.tar.gz" "syedge-node-$VER-linux-amd64"

# 假的国密基础包（与真实包一样，目录树以编译前缀为根）
mkdir -p "$W/gm$PREFIX/sbin"
printf 'gmssl binary SyEdge/1.30.6\n' > "$W/gm$PREFIX/sbin/syedge"
tar -C "$W/gm" -czf "$W/gm.tar.gz" "${PREFIX#/}"

# 假的 Lua 附加包（未改写版本的原始包：版本与数据面一致，标准版与国密版通用）
mkdir -p "$W/lua/addon/lualib"
printf '{"name":"syedge-lua","version":"1.30.6","syedge":"1.30.6"}\n' > "$W/lua/addon/VERSION"
printf '%s\n' "$PREFIX" > "$W/lua/addon/PREFIX"
printf 'syedge-lua 1.30.6\n' > "$W/lua/addon/NOTICE"
cp dataplane/lua/install.sh dataplane/lua/uninstall.sh "$W/lua/"
tar -C "$W/lua" -czf "$W/lua.tar.gz" install.sh uninstall.sh addon

SYEDGE_DATAPLANE_VERSION=1.30.6 OUT="$W/out" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" > "$W/pack.log" 2>&1 \
  || { cat "$W/pack.log"; echo "FAIL package.sh all 失败"; exit 1; }
PKG="$W/out/syedge-all-$VER-linux-amd64.tar.gz"
[[ -f "$PKG" ]] && ok "产出 syedge-all-$VER-linux-amd64.tar.gz" || { bad "没有产出整合包"; exit 1; }

A="$W/x"; mkdir -p "$A"; tar -C "$A" -xzf "$PKG"
D="$A/syedge-all-$VER-linux-amd64"
[[ -d "$D" ]] && ok "解压目录名为 syedge-all-<版本>-linux-<架构>" || bad "解压目录名不对：$(ls "$A")"
[[ -x "$D/install.sh" && -x "$D/uninstall.sh" ]] && ok "含 install.sh 与 uninstall.sh" || bad "缺少 install.sh / uninstall.sh"
[[ $(cat "$D/rootfs$PREFIX/sbin/syedge") == 'standard binary SyEdge/1.30.6' ]] && ok "rootfs 里是标准版二进制" || bad "rootfs 二进制不是标准版"
[[ $(cat "$D/extras/gmssl/syedge" 2>/dev/null) == 'gmssl binary SyEdge/1.30.6' && -x "$D/extras/gmssl/syedge" ]] && ok "extras/gmssl/syedge 是国密版二进制" || bad "缺少国密版二进制"
[[ -f "$D/extras/lua/install.sh" && -f "$D/extras/lua/uninstall.sh" && -f "$D/extras/lua/addon/VERSION" ]] && ok "extras/lua 是完整的附加包" || bad "extras/lua 不完整"
grep -q '"version":"1.30.6"' "$D/extras/lua/addon/VERSION" && ok "附加包版本保持原样（标准版与国密版通用）" || bad "附加包版本被改写"
[[ -f "$D/README.txt" ]] && grep -q -- '--gmssl' "$D/README.txt" && ok "README 说明了变体选择" || bad "README 缺少 --gmssl 说明"
if tar -tzf "$PKG" | grep -q 'extras/lua/addon/.*\.\./'; then bad "包内含上级目录路径"; else ok "包内没有 .. 路径"; fi

# install.sh 的参数边界：全部发生在要求 root 之前，普通用户即可验证
run() { bash "$D/install.sh" "$@" 2>&1 || true; }
out=$(run --gmssl);                  [[ "$out" == *"请以 root"* ]] && ok "--gmssl 在整合包中被接受（随后因非 root 停止）" || bad "--gmssl 在整合包中不应被拒绝：$out"
out=$(run --no-lua);                 [[ "$out" == *"请以 root"* ]] && ok "--no-lua 被接受" || bad "--no-lua 被拒绝：$out"
out=$(run --lua-standalone);         [[ "$out" == *"请以 root"* ]] && ok "--lua-standalone 被接受" || bad "--lua-standalone 被拒绝：$out"
out=$(run --no-lua --lua-standalone); [[ "$out" == *"互斥"* ]] && ok "--no-lua 与 --lua-standalone 互斥" || bad "互斥参数没有被拒绝：$out"
out=$(run --lua-standalone --server h:1 --token t --ca-fingerprint f); [[ "$out" == *"独立模式"* && "$out" != *"请以 root"* ]] && ok "--lua-standalone 不能与控制台注册同时使用" || bad "独立模式与注册没有互斥：$out"

# SkyWalking：包内文件、渲染脚本与 install.sh 参数边界
SW="$D/extras/skywalking"
[[ -f "$SW/render.sh" && -f "$SW/install-lib.sh" && -f "$SW/10-skywalking.conf" && -f "$SW/lualib/skywalking/tracer.lua" && -f "$SW/lualib/prometheus.lua" && -f "$SW/LUA-PLACEMENT.md" ]] && ok "extras/skywalking 含渲染脚本、初始化模板、库与说明" || bad "extras/skywalking 不完整"
[[ -z "$(find "$SW/lualib" -name '*_test.lua')" ]] && ok "SkyWalking 库不含测试文件" || bad "SkyWalking 库含测试文件"
R="$W/render"; mkdir -p "$R"
bash "$SW/render.sh" "$SW/10-skywalking.conf" http://oap.example.com:22800 gw-svc node_1 "$R/ok.conf" >/dev/null 2>&1   && grep -qF 'local OAP_ADDRESS   = "http://oap.example.com:22800"' "$R/ok.conf"   && grep -qF 'local SERVICE_NAME  = "gw-svc"' "$R/ok.conf"   && grep -qF 'local INSTANCE_NAME = "node_1"' "$R/ok.conf" \
  && ok "render.sh 把三个值写入模板" || bad "render.sh 没有正确写入三个值"
changed=$(diff "$SW/10-skywalking.conf" "$R/ok.conf" | grep -c '^>' || true)
[[ "$changed" == 3 ]] && ok "render.sh 只改了三行，其余与模板完全一致" || bad "render.sh 改动了 $changed 行（应为 3）"
for bad_oap in 'http://a;b:1' 'http://h/path' 'ftp://h:1' 'http://h:0' 'http://h:99999' 'http://192.0.2.10:22800' 'http://u@h:1' $'http://h:1\nx' 'http://h:1"--'; do
  rm -f "$R/x.conf"
  if bash "$SW/render.sh" "$SW/10-skywalking.conf" "$bad_oap" s i "$R/x.conf" >/dev/null 2>&1 || [[ -e "$R/x.conf" ]]; then bad "不合法的 OAP 地址被接受：$bad_oap"; else ok "拒绝不合法的 OAP 地址：${bad_oap//$'\n'/\\n}"; fi
done
for bad_name in 'a"b' 'a b' 'a;b' '$(id)' '`id`' $'a\nb' 'a\\b' 'a&b' '' "$(printf 'x%.0s' $(seq 65))" 'a")os.execute("x")--'; do
  rm -f "$R/x.conf"
  if bash "$SW/render.sh" "$SW/10-skywalking.conf" http://h:1 "$bad_name" i "$R/x.conf" >/dev/null 2>&1 || [[ -e "$R/x.conf" ]]; then bad "不合法的服务名被接受：$bad_name"; else ok "拒绝不合法的服务名：${bad_name//$'\n'/\\n}"; fi
  rm -f "$R/x.conf"
  if bash "$SW/render.sh" "$SW/10-skywalking.conf" http://h:1 s "$bad_name" "$R/x.conf" >/dev/null 2>&1 || [[ -e "$R/x.conf" ]]; then bad "不合法的实例名被接受：$bad_name"; else ok "拒绝不合法的实例名：${bad_name//$'\n'/\\n}"; fi
done
SWARGS=(--skywalking-oap http://h:22800 --skywalking-service s --skywalking-instance i)
out=$(run --lua-standalone "${SWARGS[@]}");  [[ "$out" == *"请以 root"* ]] && ok "三个 SkyWalking 参数加 --lua-standalone 被接受" || bad "合法的 SkyWalking 参数被拒绝：$out"
out=$(run "${SWARGS[@]}");                   [[ "$out" == *"请以 root"* ]] && ok "只给 SkyWalking 三个参数即可（自动按独立模式处理）" || bad "SkyWalking 参数没有被自动按独立模式处理：$out"
out=$(run --no-lua "${SWARGS[@]}");          [[ "$out" == *"--no-lua"* && "$out" != *"请以 root"* ]] && ok "SkyWalking 参数与 --no-lua 冲突被拒绝" || bad "SkyWalking 与 --no-lua 没有冲突：$out"
out=$(run "${SWARGS[@]}" --server h:1 --token t --ca-fingerprint f); [[ "$out" == *"独立模式"* && "$out" != *"请以 root"* ]] && ok "SkyWalking 参数不能与控制台注册同时使用" || bad "SkyWalking 与 --server 没有互斥：$out"
out=$(run --lua-standalone --skywalking-oap http://h:1); [[ "$out" == *"同时提供"* && "$out" != *"请以 root"* ]] && ok "只给部分 SkyWalking 参数时被拒绝" || bad "部分参数没有被拒绝：$out"
out=$(run --lua-standalone --skywalking-oap 'http://h:1;rm' --skywalking-service s --skywalking-instance i); [[ "$out" == *"OAP 地址"* && "$out" != *"请以 root"* ]] && ok "install.sh 在要求 root 之前就校验 OAP 地址" || bad "install.sh 没有校验 OAP 地址：$out"
out=$(run --lua-standalone --skywalking-oap http://h:1 --skywalking-service 'a"b' --skywalking-instance i); [[ "$out" == *"服务名"* && "$out" != *"请以 root"* ]] && ok "install.sh 在要求 root 之前就校验服务名" || bad "install.sh 没有校验服务名：$out"
out=$(run --lua-standalone --skywalking-oap http://h:1 --skywalking-service s --skywalking-instance 'a b'); [[ "$out" == *"实例名"* && "$out" != *"请以 root"* ]] && ok "install.sh 在要求 root 之前就校验实例名" || bad "install.sh 没有校验实例名：$out"

# 普通节点包（没有 extras）收到整合包专属参数时要明确拒绝，不能悄悄装成标准版
mkdir -p "$W/plain"; tar -C "$W/plain" -xzf "$W/node.tar.gz"
P="$W/plain/syedge-node-$VER-linux-amd64"
out=$(bash "$P/install.sh" --gmssl 2>&1 || true);  [[ "$out" == *"整合包"* && "$out" != *"请以 root"* ]] && ok "节点包不含国密版时 --gmssl 被拒绝并指向整合包" || bad "节点包没有拒绝 --gmssl：$out"
out=$(bash "$P/install.sh" --lua-standalone 2>&1 || true); [[ "$out" == *"整合包"* && "$out" != *"请以 root"* ]] && ok "节点包不含 Lua 时 --lua-standalone 被拒绝" || bad "节点包没有拒绝 --lua-standalone：$out"
out=$(bash "$P/install.sh" --lua-standalone "${SWARGS[@]}" 2>&1 || true); [[ "$out" == *"整合包"* && "$out" != *"请以 root"* ]] && ok "节点包不含 SkyWalking 库时相关参数被拒绝" || bad "节点包没有拒绝 SkyWalking 参数：$out"
out=$(bash "$P/install.sh" --no-lua 2>&1 || true); [[ "$out" == *"请以 root"* ]] && ok "节点包上 --no-lua 无害" || bad "节点包上的 --no-lua 被拒绝：$out"

# 打包本身的边界
if SYEDGE_DATAPLANE_VERSION=1.30.6 OUT="$W/out2" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/node.tar.gz" >/dev/null 2>&1; then bad "把不是附加包的文件当 Lua 附加包也被接受"; else ok "Lua 附加包结构不对时拒绝打包"; fi
if SYEDGE_DATAPLANE_VERSION=1.30.6 OUT="$W/out3" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/lua.tar.gz" "$W/lua.tar.gz" >/dev/null 2>&1; then bad "没有国密版二进制的包也被接受"; else ok "国密包里没有 sbin/syedge 时拒绝打包"; fi
# 数据面版本与 Lua 附加包的目标版本必须一致（SYEDGE_DATAPLANE_VERSION 由发布流水线给出，与 lua-upgrade 的规则相同）
if SYEDGE_DATAPLANE_VERSION=1.99.0 OUT="$W/out4" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" >"$W/mismatch.log" 2>&1; then bad "数据面 1.99.0 与 Lua 目标版本 1.30.6 不一致却被打包"; else grep -q '1.30.6' "$W/mismatch.log" && ok "数据面与 Lua 附加包版本不一致时拒绝打包" || bad "拒绝打包但没有说明版本：$(cat "$W/mismatch.log")"; fi
if SYEDGE_DATAPLANE_VERSION=1.30.6-1 OUT="$W/out5" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" >/dev/null 2>&1; then ok "数据面 1.30.6-1 与 Lua 目标 1.30.6 一致（发行序号不影响）"; else bad "版本一致的组合被拒绝"; fi
if SYEDGE_DATAPLANE_VERSION=bogus OUT="$W/out6" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" >/dev/null 2>&1; then bad "格式不对的数据面版本被接受"; else ok "格式不对的数据面版本被拒绝"; fi
# 必须给出数据面版本：缺省时不能退化为“只告警”而绕过检查
if OUT="$W/out7" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" >"$W/unset.log" 2>&1; then bad "未设置 SYEDGE_DATAPLANE_VERSION 也能出整合包"; else grep -q 'SYEDGE_DATAPLANE_VERSION' "$W/unset.log" && ok "未设置 SYEDGE_DATAPLANE_VERSION 时拒绝打包" || bad "拒绝了但没说明原因：$(cat "$W/unset.log")"; fi
# 二进制本身的版本也要核对：国密版、标准版与声明的数据面版本不一致都拒绝
mkdir -p "$W/gmbad$PREFIX/sbin"; printf 'gmssl binary SyEdge/1.99.0\n' > "$W/gmbad$PREFIX/sbin/syedge"; tar -C "$W/gmbad" -czf "$W/gmbad.tar.gz" "${PREFIX#/}"
if SYEDGE_DATAPLANE_VERSION=1.30.6 OUT="$W/out8" VERSION=$VER scripts/release/package.sh all amd64 "$W/node.tar.gz" "$W/gmbad.tar.gz" "$W/lua.tar.gz" >"$W/gmbad.log" 2>&1; then bad "国密版二进制版本 1.99.0 与数据面 1.30.6 不一致却被打包"; else grep -q '1.99.0' "$W/gmbad.log" && ok "国密版二进制版本不一致时拒绝打包" || bad "拒绝了但没说明版本：$(cat "$W/gmbad.log")"; fi
mkdir -p "$W/nodebad/syedge-node-$VER-linux-amd64/rootfs$PREFIX/sbin"; cp -r "$N/." "$W/nodebad/syedge-node-$VER-linux-amd64/"; printf 'standard binary SyEdge/1.99.0\n' > "$W/nodebad/syedge-node-$VER-linux-amd64/rootfs$PREFIX/sbin/syedge"
tar -C "$W/nodebad" -czf "$W/nodebad.tar.gz" "syedge-node-$VER-linux-amd64"
if SYEDGE_DATAPLANE_VERSION=1.30.6 OUT="$W/out9" VERSION=$VER scripts/release/package.sh all amd64 "$W/nodebad.tar.gz" "$W/gm.tar.gz" "$W/lua.tar.gz" >"$W/nodebad.log" 2>&1; then bad "节点包二进制版本不一致却被打包"; else grep -q '1.99.0' "$W/nodebad.log" && ok "节点包二进制版本不一致时拒绝打包" || bad "拒绝了但没说明版本：$(cat "$W/nodebad.log")"; fi

echo "---- $fail failed"
[[ $fail -eq 0 ]] || exit 1
echo 'syedge-all 整合包结构与 install.sh 参数：OK'
