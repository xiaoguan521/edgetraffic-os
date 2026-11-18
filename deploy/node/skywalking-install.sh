# SkyWalking 库与初始化文件的安装步骤（被 install.sh 引用，不单独运行）。
#   sw_precheck R SWSRC CONFD
#       只读检查：库文件是否与已有的不同名同内容、conf.d 是否已有同级别重复指令。失败时在 stderr 说明原因并返回 1，不写任何文件。
#   sw_apply R SWSRC STAGED_CONF CONFD ERRFILE
#       先再检查一次，再写入库与 10-skywalking.conf 并用 syedge -t 校验；校验失败则还原写入前的状态
#       （用户原有的 10-skywalking.conf 放回原处，本次新增的库文件删除），返回 1。
# 失败时都只返回非 0，由调用方决定如何退出。

sw_precheck() {
  local R=$1 SWSRC=$2 CONFD=$3 f rel dup
  local conflicts=()
  [[ -d "$CONFD" ]] || { echo "找不到 $CONFD" >&2; return 1; }
  while IFS= read -r -d '' f; do
    rel=${f#"$SWSRC"/}
    if [[ -e "$R/lualib/$rel" ]] && ! cmp -s "$f" "$R/lualib/$rel"; then conflicts+=("$rel"); fi
  done < <(find "$SWSRC" -type f -name '*.lua' -print0)
  if [[ ${#conflicts[@]} -ne 0 ]]; then
    echo "$R/lualib 下已有内容不同的同名文件，为避免覆盖你自己的库而停止：${conflicts[*]}" >&2
    return 1
  fi
  dup=$(grep -lE 'init_worker_by_lua_block|lua_shared_dict[[:space:]]+tracing_buffer' "$CONFD"/*.conf 2>/dev/null | grep -v '/10-skywalking.conf$' || true)
  if [[ -n "$dup" ]]; then
    echo "conf.d 里已有文件定义了 init_worker_by_lua_block 或 tracing_buffer，同级别重复会让配置校验失败：$dup" >&2
    return 1
  fi
}

sw_apply() {
  local R=$1 SWSRC=$2 STAGED=$3 CONFD=$4 ERR=$5 f rel dest bak="" own=()
  local created=()
  sw_precheck "$R" "$SWSRC" "$CONFD" || return 1
  [[ $EUID -ne 0 ]] || own=(-o root -g root)
  if [[ -e "$CONFD/10-skywalking.conf" ]]; then
    bak=$(mktemp "$CONFD/.10-skywalking.conf.bak.XXXXXX") || return 1
    cp -p "$CONFD/10-skywalking.conf" "$bak" || { rm -f "$bak"; return 1; }
  fi
  while IFS= read -r -d '' f; do
    rel=${f#"$SWSRC"/}; dest="$R/lualib/$rel"
    [[ -e "$dest" ]] || created+=("$dest")
    mkdir -p "$(dirname "$dest")"
    install ${own[@]+"${own[@]}"} -m 0644 "$f" "$dest"
  done < <(find "$SWSRC" -type f -name '*.lua' -print0)
  install ${own[@]+"${own[@]}"} -m 0644 "$STAGED" "$CONFD/10-skywalking.conf"
  if ! "$R/sbin/syedge" -p "$R" -c "$R/conf/current/syedge.conf" -t >/dev/null 2>"$ERR"; then
    if [[ -n "$bak" ]]; then mv -f "$bak" "$CONFD/10-skywalking.conf"; else rm -f "$CONFD/10-skywalking.conf"; fi
    if [[ ${#created[@]} -gt 0 ]]; then rm -f "${created[@]}"; fi
    echo "加入 SkyWalking 初始化后配置校验失败，已还原：$(tail -n 3 "$ERR" | tr '\n' ' ')" >&2
    return 1
  fi
  [[ -z "$bak" ]] || rm -f "$bak"
}
