# SkyWalking 库与初始化文件的安装步骤（被 install.sh 引用，不单独运行）。
#   sw_precheck R SWSRC CONFD
#       只读检查：库文件是否与已有的不同名同内容、conf.d 是否已有同级别重复指令。失败时在 stderr 说明原因并返回 1，不写任何文件。
#   sw_apply R SWSRC STAGED_CONF CONFD ERRFILE
#       先再检查一次，再写入库与 10-skywalking.conf 并用 syedge -t 校验；校验失败则还原写入前的状态
#       （用户原有的 10-skywalking.conf 放回原处，本次新增的库文件删除），返回 1。
#   sw_snapshot_conf CONFD SNAP / sw_restore_conf CONFD SNAP
#       sw_apply 之前记下已有的 10-skywalking.conf（没有则不留快照）；之后 reload 失败时用 sw_restore_conf 放回，
#       没有快照说明原先没有该文件，删除本次写入的。库文件不处理：没有初始化文件它们不会被加载。
# 失败时都只返回非 0，由调用方决定如何退出。

sw_precheck() {
  local R=$1 SWSRC=$2 CONFD=$3 f rel dup
  local conflicts=()
  # 全新节点的 conf.d 由随后的 Lua 安装步骤创建，此时还不存在：没有文件就不可能有重复指令，只检查库文件
  while IFS= read -r -d '' f; do
    rel=${f#"$SWSRC"/}
    if [[ -e "$R/lualib/$rel" ]] && ! cmp -s "$f" "$R/lualib/$rel"; then conflicts+=("$rel"); fi
  done < <(find "$SWSRC" -type f -name '*.lua' -print0)
  if [[ ${#conflicts[@]} -ne 0 ]]; then
    echo "$R/lualib 下已有内容不同的同名文件，为避免覆盖你自己的库而停止：${conflicts[*]}" >&2
    return 1
  fi
  [[ -d "$CONFD" ]] || return 0
  dup=$(grep -lE 'init_worker_by_lua_block|lua_shared_dict[[:space:]]+tracing_buffer' "$CONFD"/*.conf 2>/dev/null | grep -v '/10-skywalking.conf$' || true)
  if [[ -n "$dup" ]]; then
    echo "conf.d 里已有文件定义了 init_worker_by_lua_block 或 tracing_buffer，同级别重复会让配置校验失败：$dup" >&2
    return 1
  fi
}

sw_apply() {
  local R=$1 SWSRC=$2 STAGED=$3 CONFD=$4 ERR=$5 f rel dest bak="" own=() created=() i
  # 调用方常把本函数放在 ||、if 里，此时 set -e 不生效：每一步都必须显式检查，失败走 sw_undo
  [[ -d "$CONFD" ]] || { echo "找不到 $CONFD" >&2; return 1; }
  sw_precheck "$R" "$SWSRC" "$CONFD" || return 1
  [[ $EUID -ne 0 ]] || own=(-o root -g root)
  if [[ -e "$CONFD/10-skywalking.conf" ]]; then
    bak=$(mktemp "$CONFD/.10-skywalking.conf.bak.XXXXXX") || return 1
    cp -p "$CONFD/10-skywalking.conf" "$bak" || { rm -f "$bak"; echo "无法备份已有的 10-skywalking.conf" >&2; return 1; }
  fi
  sw_undo() {  # 还原 10-skywalking.conf，删除本次新增的库文件
    if [[ -n "$bak" ]]; then mv -f "$bak" "$CONFD/10-skywalking.conf"; else rm -f "$CONFD/10-skywalking.conf"; fi
    for i in ${created[@]+"${created[@]}"}; do rm -f "$i"; done
  }
  while IFS= read -r -d '' f; do
    rel=${f#"$SWSRC"/}; dest="$R/lualib/$rel"
    [[ -e "$dest" ]] || created+=("$dest")
    if ! { mkdir -p "$(dirname "$dest")" && install ${own[@]+"${own[@]}"} -m 0644 "$f" "$dest"; }; then
      sw_undo; echo "写入 $dest 失败，已还原" >&2; return 1
    fi
  done < <(find "$SWSRC" -type f -name '*.lua' -print0)
  if ! install ${own[@]+"${own[@]}"} -m 0644 "$STAGED" "$CONFD/10-skywalking.conf"; then
    sw_undo; echo "写入 $CONFD/10-skywalking.conf 失败，已还原" >&2; return 1
  fi
  if ! "$R/sbin/syedge" -p "$R" -c "$R/conf/current/syedge.conf" -t >/dev/null 2>"$ERR"; then
    sw_undo; echo "加入 SkyWalking 初始化后配置校验失败，已还原：$(tail -n 3 "$ERR" | tr '\n' ' ')" >&2; return 1
  fi
  [[ -z "$bak" ]] || rm -f "$bak"
  return 0
}

sw_snapshot_conf() {
  local CONFD=$1 SNAP=$2
  rm -f -- "$SNAP" || return 1
  [[ ! -e "$CONFD/10-skywalking.conf" ]] || cp -p -- "$CONFD/10-skywalking.conf" "$SNAP"
}

sw_restore_conf() {
  local CONFD=$1 SNAP=$2
  if [[ -d "$CONFD/10-skywalking.conf" ]]; then return 1; fi  # mv 会把文件放进同名目录而不是报错
  if [[ -e "$SNAP" ]]; then
    cp -p -- "$SNAP" "$CONFD/10-skywalking.conf.restore.$$" && mv -f -- "$CONFD/10-skywalking.conf.restore.$$" "$CONFD/10-skywalking.conf" || { rm -f -- "$CONFD/10-skywalking.conf.restore.$$"; return 1; }
  else
    rm -f -- "$CONFD/10-skywalking.conf" || return 1
  fi
}
