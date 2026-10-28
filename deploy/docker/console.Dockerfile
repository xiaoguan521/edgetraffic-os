# 发布镜像：控制台。由 .github/workflows/release.yml 用预先编译好的二进制按架构原生构建，
# 构建上下文为发布暂存目录（console/ 下是 syapp-console 与 syapp-license-server）。
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY console/ /usr/bin/
COPY NOTICE /usr/share/doc/edgetraffic-os/NOTICE
VOLUME /var/lib/syapp-console
ENV ETOS_DATA=/var/lib/syapp-console
EXPOSE 8443 9443
ENTRYPOINT ["/usr/bin/syapp-console"]
CMD ["-listen", "0.0.0.0:8443", "-grpc-listen", "0.0.0.0:9443"]
