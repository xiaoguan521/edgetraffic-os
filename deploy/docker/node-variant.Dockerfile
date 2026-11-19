# 发布镜像变体：在标准版 node 镜像上叠加 Lua 附加包（node-lua），或再换成国密版数据面（node-gm-lua）。
# 构建上下文（发布流水线准备）：
#   lua/   Lua 附加包解包目录（含 install.sh、uninstall.sh、addon/），两种变体都有
#   gm/syedge   国密版二进制，仅 node-gm-lua 有
# 与整合包安装器同一条路径：先换二进制，再运行附加包自带的 install.sh（按 -V 自动选标准版或国密版模块，并用真实 syedge -t 自检）。
ARG BASE
FROM ${BASE}
COPY . /tmp/variant
RUN set -eu; \
    if [ -f /tmp/variant/gm/syedge ]; then \
      install -o root -g root -m 0755 /tmp/variant/gm/syedge "${SYEDGE_PREFIX}/sbin/syedge.gm" \
      && mv -f "${SYEDGE_PREFIX}/sbin/syedge.gm" "${SYEDGE_PREFIX}/sbin/syedge" \
      && "${SYEDGE_PREFIX}/sbin/syedge" -V 2>&1 | grep -q enable-ntls; \
    fi; \
    bash /tmp/variant/lua/install.sh; \
    mkdir -p /usr/local/share/syedge-lua && cp -r /tmp/variant/lua/. /usr/local/share/syedge-lua/ \
    && rm -rf /tmp/variant
