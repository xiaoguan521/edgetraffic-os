# 发布镜像：演示用上游业务服务（order-api）。
FROM ubuntu:24.04
COPY backend/demo-backend /usr/bin/demo-backend
EXPOSE 9001
ENTRYPOINT ["/usr/bin/demo-backend"]
