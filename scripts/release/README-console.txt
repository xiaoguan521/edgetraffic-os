SyEdge 控制台
=====================

本包包含：
  syedge-console          控制台（Web 管理界面已内置）
  syedge-license-server   演示授权签发工具（只在开发 / 演示构建中提供；正式版本不包含）

快速试用（数据保存在 ./data，SQLite）：

  ./syedge-console -listen 0.0.0.0:8443 -grpc-listen 0.0.0.0:9443 \
      -grpc-hosts <本机IP或域名> -grpc-advertise <本机IP或域名>:9443 -data ./data

浏览器打开 http://<本机>:8443 创建管理员，导入授权后在“节点”页添加边缘节点。
边缘节点使用 syedge-node-<版本>-linux-<架构>.tar.gz 安装（见其中 README.txt）。

HTTPS：加 -tls-auto（或环境变量 ETOS_TLS_AUTO=1），控制台用内置 Web CA 自动签发并续期证书；
首次访问时下载 https://<控制台>/web-ca.crt 导入浏览器即可。已有企业证书时用 -tls-cert / -tls-key。

正式部署建议：
  - Linux 上使用 syedge-console.service（systemd），环境变量写入 /etc/syedge-console/console.env
  - Web 端口只在内网开放，并开启 HTTPS（-tls-auto 或 -tls-cert / -tls-key）
  - 生产环境建议使用 PostgreSQL（-db postgres://...），每次发布都在 PostgreSQL 16 上做测试
  - 节点连接端口 9443 对边缘节点开放（mTLS）

备份与恢复（加密，包含数据库、节点 CA、私钥加密密钥与安装 ID）：

  # 口令写入文件（不要放在命令行参数里），至少 12 个字符，丢失后备份无法解开
  ./syedge-console backup  -data ./data -passphrase-file /root/etos-backup.pass -out console-$(date +%F).etosbk
  # 恢复到新的数据目录 / 空数据库（控制台未运行时执行），然后正常启动控制台
  ./syedge-console restore -data ./data-new [-db postgres://...] -passphrase-file /root/etos-backup.pass -in console-2026-10-29.etosbk

  - 备份可在控制台运行时执行，所有表在同一事务中读取，内容一致
  - 默认不含流量统计，需要时加 -metrics
  - 同一份备份可恢复到 SQLite 或 PostgreSQL，这也是 SQLite 迁移到 PostgreSQL 的方法
  - 恢复后安装 ID 不变，原授权继续有效；节点证书由同一 CA 签发，节点自动重连
  - 备份文件含 CA 私钥，请与口令分开保存

Based on NGINX Open Source (BSD-2-Clause)，详见 NOTICE。
