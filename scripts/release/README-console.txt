EdgeTraffic OS 控制台
=====================

本包包含：
  syapp-console          控制台（Web 管理界面已内置）
  syapp-license-server   授权签发工具（仅授权方使用，需要签名私钥）

快速试用（数据保存在 ./data，SQLite）：

  ./syapp-console -listen 0.0.0.0:8443 -grpc-listen 0.0.0.0:9443 -tls-auto \
      -grpc-hosts <本机IP或域名> -grpc-advertise <本机IP或域名>:9443 -data ./data

浏览器打开 https://<本机>:8443（自签证书，核对启动日志中的指纹后继续访问） 创建管理员，导入授权后在“节点”页添加边缘节点。
边缘节点使用 edgetraffic-os-node-<版本>-linux-<架构>.tar.gz 安装（见其中 README.txt）。

正式部署建议：
  - Linux 上用 syapp-console 软件包（RPM / DEB）安装，配置写在 /etc/syapp-console/console.env
  - HTTPS：加 -tls-auto 自动生成自签证书（启动日志打印 SHA-256 指纹供浏览器核对），
    或用 -tls-cert / -tls-key 指定受信任的证书；软件包默认已开启 -tls-auto
  - 数据库：默认 SQLite（<数据目录>/console.db）；多实例或较大规模用 PostgreSQL 14+：
    -db "postgres://用户:密码@主机:5432/库名?sslmode=require"
  - Web 端口只在内网开放；节点连接端口 9443 对边缘节点开放（mTLS）

Based on NGINX Open Source (BSD-2-Clause)，详见 NOTICE。
