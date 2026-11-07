# 发布镜像：演示用边缘节点（容器模拟一台装好节点包的服务器）。
# 构建上下文为发布暂存目录：node/rootfs 是节点发布包的 rootfs，node/ 下另有演示入口脚本。
# syedge 在 Ubuntu 22.04 上编译，运行时基础镜像保持一致。
FROM ubuntu:22.04
RUN apt-get update && apt-get install -y --no-install-recommends libpcre2-8-0 libssl3 zlib1g sudo \
      ca-certificates curl procps && rm -rf /var/lib/apt/lists/*
RUN groupadd -r syedge && useradd -r -g syedge -s /usr/sbin/nologin syedge \
 && useradd -r -g syedge -s /usr/sbin/nologin syedge-agent
COPY node/rootfs/opt/syedge /opt/syedge
COPY node/rootfs/usr/bin/syedge-agent /usr/bin/syedge-agent
COPY node/rootfs/usr/libexec/syedge/syedge-wrapper /usr/libexec/syedge/syedge-wrapper
COPY node/rootfs/etc/sudoers.d/syedge-agent /etc/sudoers.d/syedge-agent
RUN chmod 0440 /etc/sudoers.d/syedge-agent && \
 (visudo -c || (sed -i '/^Defaults:[^[:space:]]* !requiretty$/d' /etc/sudoers.d/syedge-agent && visudo -c)) && \
 ln -s /opt/syedge/sbin/syedge /usr/sbin/syedge \
 && /opt/syedge/sbin/syedge -v
COPY node/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY node/systemctl /usr/local/bin/systemctl
EXPOSE 80 443
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
