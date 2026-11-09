R=$(syedge_root)
if [ "$R" != /opt/syedge ]; then
  mkdir -p "$R/sbin" "$R/share" "$R/html" "$R/modules"
  cp -a /opt/syedge/sbin/. "$R/sbin/"
  cp -a /opt/syedge/share/. "$R/share/"
  cp -a /opt/syedge/html/. "$R/html/"
  if [ -d /opt/syedge/modules ]; then cp -a /opt/syedge/modules/. "$R/modules/"; fi
fi
if [ "$R" = /opt/syedge ]; then
  LOG_DIR=/var/log/syedge
  CACHE_DIR=/var/cache/syedge
else
  LOG_DIR="$R/logs"
  CACHE_DIR="$R/cache"
fi
mkdir -p "$R/conf/releases" "$LOG_DIR" "$CACHE_DIR" /run/syedge /run/syedge-agent "$R/versions" "$R/modules"
ln -sfn "$R/sbin/syedge" /usr/sbin/syedge
chown -R root:root "$R/sbin" "$R/share" "$R/html"

# 包管理器在 syedge-agent 包之前执行本脚本，custom root 的首次基线不能直接
# 复用按 /opt 编译的静态配置：日志和临时目录必须先改到 R 下。后续升级不改
# current，因此只在真正写入 r000000 前处理这份随包的模板。
if [ "$R" != /opt/syedge ] && [ -f "$R/share/defaults/baseline/syedge.conf" ]; then
  sed -i "s|/var/log/syedge|$R/logs|g" "$R/share/defaults/baseline/syedge.conf"
  sed -i "/^[[:space:]]*client_header_timeout/i\\    client_body_temp_path  $R/cache/client_temp;\n    proxy_temp_path        $R/cache/proxy_temp;\n    fastcgi_temp_path      $R/cache/fastcgi_temp;\n    uwsgi_temp_path        $R/cache/uwsgi_temp;\n    scgi_temp_path         $R/cache/scgi_temp;" "$R/share/defaults/baseline/syedge.conf"
fi
# 运行时不使用 root（设计 0001 D5）：master 与 worker 以 syedge 运行，运行时目录、日志与缓存归 syedge；
# 二进制所在目录、版本库与模块目录由 wrapper（同样以 syedge 运行）在热升级时写入
chown -R syedge:syedge "$LOG_DIR" "$CACHE_DIR" /run/syedge "$R/versions"
chown syedge:syedge "$R/sbin" "$R/modules"
if [ -x /usr/sbin/syedge-instance ]; then
  # 与 syedge-agent 同时升级：升级前以 root 启动的 master 无中断切换为 syedge 运行（旧版 agent 包没有该命令时忽略）
  /usr/sbin/syedge-instance nonroot >/dev/null 2>&1 || true
elif [ -f /run/syedge/syedge.pid ] && [ "$(awk '/^Uid:/{print $2}' /proc/"$(cat /run/syedge/syedge.pid)"/status 2>/dev/null)" = "0" ]; then
  echo "提示：运行中的 syedge master 仍以 root 运行（升级前启动）。安装 syedge-agent 后会自动无中断切换；" >&2
  echo "      独立使用时请在维护窗口执行 systemctl restart syedge，之后以 syedge 用户运行。" >&2
fi
if has_systemd; then
  systemctl daemon-reload || true
  if systemctl is-active -q syedge.service; then
    # 升级：只 reload，绝不 restart。新二进制在下一次二进制热升级时生效，运行中的连接不受影响。
    systemctl reload syedge.service || true
  fi
fi
# 全新安装：写入初始版本 r000000（与 syedge-agent bootstrap 生成的完全相同），syedge 可独立使用。
# 已有 current（升级、或已由 agent 管理）时不改动任何配置。
if [ ! -e "$R/conf/current" ] && [ -f "$R/share/defaults/baseline/syedge.conf" ]; then
  if [ ! -e "$R/conf/releases/r000000" ]; then
    cp -a "$R/share/defaults/baseline" "$R/conf/releases/r000000.new"
    mv "$R/conf/releases/r000000.new" "$R/conf/releases/r000000"
  fi
  mkdir -p "$R/conf/releases/r000000/conf.d"
  ln -sfn releases/r000000 "$R/conf/current"
  if [ "$R" != /opt/syedge ]; then
    # syedge-agent 包随后会用带 Profile.Root 的渲染器重建 r000000，修正 MANIFEST
    # 并保留 custom root 的完整路径；没有 agent 包时这份基线也能独立启动。
    : > "$R/conf/.syedge-custom-baseline"
  fi
  echo "syedge 已安装，并写入了初始配置（未匹配域名的请求直接断开）。"
  echo "  独立使用：systemctl enable syedge && systemctl start syedge，站点配置放在 $R/conf/current/conf.d/，"
  echo "            示例 $R/share/defaults/standalone-example.conf，说明 $R/share/doc/STANDALONE.txt"
  echo "  接入控制台：安装 syedge-agent 后在控制台“节点”页添加节点。"
fi
exit 0
