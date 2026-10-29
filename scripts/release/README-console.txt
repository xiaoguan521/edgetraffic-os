EdgeTraffic OS 控制台
=====================

本包包含：
  syapp-console          控制台（Web 管理界面已内置）
  syapp-license-server   授权签发工具（仅授权方使用，需要签名私钥）

快速试用（数据保存在 ./data，SQLite）：

  ./syapp-console -listen 0.0.0.0:8443 -grpc-listen 0.0.0.0:9443 \
      -grpc-hosts <本机IP或域名> -grpc-advertise <本机IP或域名>:9443 -data ./data

浏览器打开 http://<本机>:8443 创建管理员，导入授权后在“节点”页添加边缘节点。
边缘节点使用 edgetraffic-os-node-<版本>-linux-<架构>.tar.gz 安装（见其中 README.txt）。

HTTPS：加 -tls-auto（或环境变量 ETOS_TLS_AUTO=1），控制台用内置 Web CA 自动签发并续期证书；
首次访问时下载 https://<控制台>/web-ca.crt 导入浏览器即可。已有企业证书时用 -tls-cert / -tls-key。

正式部署建议：
  - Linux 上使用 syapp-console.service（systemd），环境变量写入 /etc/syapp-console/console.env
  - Web 端口只在内网开放，并开启 HTTPS（-tls-auto 或 -tls-cert / -tls-key）
  - 生产环境建议使用 PostgreSQL（-db postgres://...），每次发布都在 PostgreSQL 16 上做测试
  - 节点连接端口 9443 对边缘节点开放（mTLS）

Based on NGINX Open Source (BSD-2-Clause)，详见 NOTICE。
