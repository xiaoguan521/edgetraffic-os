# SyEdge 快速上手

本页只列最常用的步骤。命令与 [安装维护手册](install-maintenance.md) 第 4、5、8 节一致，完整说明以手册为准。

## 1. 选包

| 你要装什么 | 文件 | 说明 |
| --- | --- | --- |
| 控制台 | `syedge-console-<版本>-linux-<架构>.tar.gz`，或 `syedge-console` 的 RPM/DEB | 装在管理服务器上 |
| 边缘节点 | `syedge-node-<版本>-linux-<架构>.tar.gz`，或 `syedge` 与 `syedge-agent` 的 RPM/DEB | 装在每台 syedge 服务器上 |
| Lua 附加包（可选） | `syedge-lua-<数据面版本>-x86_64.tar.gz`（aarch64 同理） | 需要 `*_by_lua*` 时安装，必须与 `syedge` 版本一致 |

每个文件的用途与选择方法见 [release-assets.md](release-assets.md)。

## 2. 独立模式：5 条命令跑起来

只运行数据面、不接入控制台时使用。

① 解压并安装节点包：

```bash
tar -xzf syedge-node-<版本>-linux-<架构>.tar.gz
cd syedge-node-<版本>-linux-<架构>
sudo ./install.sh
```

安装完成后取得安装根目录，下面的路径都用变量 `R` 表示：

```bash
R=$(sudo syedge-instance prefix)
```

② 复制独立模式示例配置，然后按实际域名与后端修改：

```bash
sudo cp "$R/share/defaults/standalone-example.conf" "$R/conf/current/conf.d/www.conf"
sudo vi "$R/conf/current/conf.d/www.conf"
```

③ 校验配置（显式给出 `-p`、`-c`，任何安装根目录下都适用）：

```bash
sudo "$R/sbin/syedge" -p "$R" -c "$R/conf/current/syedge.conf" -e stderr -t
```

④ 平滑重载，不要用 restart 代替：

```bash
sudo systemctl reload syedge
```

未匹配到任何 `server_name` 的请求会按基线配置处理，不要通过删除基线配置来“放开”流量。

## 3. 接入控制台：3 步

① 在管理服务器上解压并启动控制台（参数按实际环境替换）：

```bash
tar -xzf syedge-console-<版本>-linux-<架构>.tar.gz
cd syedge-console-<版本>-linux-<架构>

./syedge-console \
  -listen 0.0.0.0:8443 \
  -grpc-listen 0.0.0.0:9443 \
  -grpc-hosts <控制台域名或IP> \
  -grpc-advertise <控制台域名或IP>:9443 \
  -tls-auto \
  -data ./data
```

首次访问前，从 `<数据目录>/pki/web-ca.crt` 或 `https://<控制台>/web-ca.crt` 下载 CA 并导入浏览器。生产环境不要把未加密的 Web 控制台暴露到公网。

② 在控制台“节点”页面点击“添加节点”，复制生成的一次性注册命令。

③ 在节点上执行该命令。等价的手工写法如下：

```bash
sudo syedge-agent enroll \
  -server <控制台域名或IP>:9443 \
  -token <一次性令牌> \
  -ca-fingerprint <控制台CA的SHA256指纹>

sudo systemctl enable syedge-agent && sudo systemctl start syedge-agent
sudo systemctl status syedge-agent --no-pager
```

节点原为独立模式且有人工维护的配置时，注册默认会被拒绝。确认由控制台接管后，再加上 `-replace-local-config`；原配置会备份到 `/var/lib/syedge-agent/backups/`。

## 4. 常用命令对照

| 操作 | OpenResty 中的写法 | SyEdge 命令 |
| --- | --- | --- |
| 校验配置 | `openresty -t` | `sudo syedge -t`；看展开后的全部配置用 `sudo syedge -T` |
| 平滑重载 | `openresty -s reload` | `sudo systemctl reload syedge`，或 `sudo syedge -s reload` |
| 查看版本与编译参数 | `openresty -v` / `openresty -V` | `syedge -v` / `syedge -V` |
| 重开日志 | `openresty -s reopen` | `sudo syedge -s reopen` |
| 查看错误日志 | `tail -f /usr/local/openresty/nginx/logs/error.log` | `sudo tail -f /var/log/syedge/error.log`；安装根目录不是 `/opt/syedge` 时为 `$R/logs/error.log` |

安装根目录不是编译期默认值时，`syedge -t` 等命令要像第 2 节那样带上 `-p` 与 `-c`。

线上配置变更优先使用 reload。`restart` 会停止并重新启动服务，可能中断现有连接。

从 OpenResty 迁移：见 [从 OpenResty 迁移](migrate-from-openresty.md)。
