# SyEdge 演示手册

在一台 Linux 服务器上，一条命令启动完整演示环境：控制台、两台边缘节点（syedge + syedge-agent）、两个演示用后端服务。全程约 15 分钟。

```
            浏览器 ──8443──▶ console（控制台）◀──9443（节点主动连接，mTLS）──┐
                                                                         │
  curl ──8001──▶ node1  edge-sg-01（syedge + agent）──┐                    │
  curl ──8002──▶ node2  edge-sg-02（syedge + agent）──┴─▶ backend1 / backend2（order-api）
```

## 准备（演示前一天完成）

三种方式任选其一，推荐 A。

| 项目 | 要求 |
| --- | --- |
| 服务器 | Linux x86_64 或 arm64，4 核 8 GB，Docker 24+ 与 docker compose 插件（方式 A / B） |
| 端口 | 8443（控制台）、8001 / 8002（两台节点）对演示电脑开放 |
| 演示密钥 | 把 `etos-demo-2026.key` 放到 `demo/keys/`（向项目负责人索取，**不要提交到仓库**） |

### 方式 A：拉取预构建镜像（推荐，约 1 分钟）

每次推送 `mvp` / `main` 分支、打 `v*` 标签，GitHub Actions 都会构建 linux/amd64 + linux/arm64 镜像并推送到 GitHub 容器仓库：

| 镜像 | 内容 |
| --- | --- |
| `ghcr.io/xiaoguan521/edgetraffic-os/console` | 控制台（含签发工具，用于现场签发演示授权） |
| `ghcr.io/xiaoguan521/edgetraffic-os/node` | 边缘节点：syedge + syedge-agent + syedge-wrapper |
| `ghcr.io/xiaoguan521/edgetraffic-os/demo-backend` | 演示用上游服务 order-api |

标签：分支名（`mvp`、`main`）、版本号（`0.1.0`）、`sha-<提交>`、`latest`（最新正式版）。

```bash
# 仓库是私有的，先登录一次（GitHub 个人令牌，勾选 read:packages）
echo <令牌> | docker login ghcr.io -u <GitHub 用户名> --password-stdin

git clone https://github.com/xiaoguan521/edgetraffic-os.git   # 只用到 demo/ 目录
cd edgetraffic-os/demo
mkdir -p keys && cp /path/to/etos-demo-2026.key keys/
./demo.sh pull            # 默认 mvp 分支最新构建；指定版本：./demo.sh pull v0.1.0
```

`pull` 会把镜像名写入 `demo/.env`，之后 `./demo.sh up / down / reset` 都使用这批镜像；`./demo.sh build` 切回本地构建。

### 方式 B：本地构建镜像（5–10 分钟）

服务器需要能访问 Docker Hub、GitHub、npm。

```bash
git clone https://github.com/xiaoguan521/edgetraffic-os.git
cd edgetraffic-os/demo
mkdir -p keys && cp /path/to/etos-demo-2026.key keys/
./demo.sh up
```

如果构建时默认 Ubuntu 软件源不可用，可以用 `UBUNTU_MIRROR` 指定支持服务器架构的镜像地址。例如，在 amd64 上使用 Ubuntu 的 Azure 镜像：

```bash
UBUNTU_MIRROR=http://azure.archive.ubuntu.com/ubuntu ./demo.sh up
```

构建后的镜像也会保留指定的软件源，APT 仍会校验软件源签名；不指定时使用基础镜像的默认源。

### 方式 C：直接用二进制，不用 Docker

到仓库 Releases 页下载（`dev-mvp` 是分支滚动构建，`v*` 是正式版），`SHA256SUMS` 可校验完整性：

| 发布包 | 平台 |
| --- | --- |
| `syedge-console-<版本>-<系统>-<架构>` | linux / darwin（tar.gz）、windows（zip），amd64 / arm64 |
| `syedge-node-<版本>-linux-<架构>.tar.gz` | Linux amd64 / arm64，systemd，glibc 2.35+、OpenSSL 3（Ubuntu 22.04+ / Debian 12+） |

```bash
gh release download dev-mvp -R xiaoguan521/edgetraffic-os -p '*linux-amd64*'

# 控制台（任意一台机器，笔记本也可以）
tar -xzf syedge-console-*-linux-amd64.tar.gz && cd syedge-console-*/
./syedge-console -listen 0.0.0.0:8443 -grpc-listen 0.0.0.0:9443 \
  -grpc-hosts <控制台IP> -grpc-advertise <控制台IP>:9443 -data ./data
# 演示授权：./syedge-license-server issue -key etos-demo-2026.key -kid etos-demo-2026 \
#   -installation-id <data/installation.json 中的 ID> -binding installation_only -customer 演示客户 -out demo.lic

# 每台边缘节点（真实 Linux 服务器）
tar -xzf syedge-node-*-linux-amd64.tar.gz && cd syedge-node-*/
sudo ./install.sh          # 安装并由 systemd 启动 syedge
# 然后把控制台“添加节点”生成的命令粘贴执行
```

方式 C 下演示流程中的 `./demo.sh enroll`、`curl :8001` 换成在节点上直接执行注册命令、`curl` 节点的 80 端口即可。

### 彩排

```bash
./e2e.sh                          # 自动把整个演示跑一遍（方式 A / B）
./demo.sh reset && ./demo.sh up   # 回到干净的初始状态
```

## 演示流程

### 1. 数据面是我们自己的产品（1 分钟）

```bash
./demo.sh ps node1
```

看点：`SyEdge version: SyEdge/1.30.6`；进程是 `SyEdge: master process` / `SyEdge: worker process`；整台机器上看不到 nginx 字样。
讲解：数据面基于 NGINX 官方源码，只改名、不改逻辑，行为与官方逐字节一致，升级上游成本低。

### 2. 初始化与授权（2 分钟）

1. 浏览器打开 `http://<服务器>:8443`，创建管理员账号。
2. 在终端签发授权：`./demo.sh license`，生成 `out/demo.lic`。
3. 在“导入授权”页选择 `demo.lic` 导入，顶栏显示“专业版，剩余 365 天”。

讲解：离线授权，签名防篡改，绑定这台控制台；授权页列出可用功能与节点、站点配额。

### 3. 添加两台边缘节点（2 分钟）

1. “节点”页点“添加节点”，复制命令。
2. 在终端执行：`./demo.sh enroll node1 <粘贴命令>`；对 node2 重复一次（每次都要重新生成命令，令牌只能用一次）。
3. 几秒内两台节点出现在列表和“总览”页，显示数据面版本、负载、心跳。

讲解：节点只主动连控制台，自己不开任何管理端口；首次连接用 CA 指纹确认控制台身份，之后全程双向证书。

### 4. 建站点并发布（3 分钟）

1. “站点与上游”页，切到“上游服务”，新建 `order-api`：服务器 `172.30.0.31:9001`、`172.30.0.32:9001`，最少连接。
2. 切回“站点”，新建站点 `shop`，域名 `shop.demo.local`，路由：
   - `/` 静态页面，内容 `<h1>演示商城 v1</h1>`
   - `/api/` 反向代理到 `order-api`
   - `/admin/` 拒绝访问
3. 可以顺手演示校验：路径里填 `/a;return 200`，或上游填 `169.254.169.254:80`，保存时直接被拒。
4. “发布”页查看逐文件 diff，填写说明“上线演示商城”，点“发布到 default”。两条节点车道依次走完 下发 → 校验 → 切换 → 加载 → 探活。
5. 验证：

```bash
curl -H 'Host: shop.demo.local' http://<服务器>:8001/          # 演示商城 v1
curl -H 'Host: shop.demo.local' http://<服务器>:8002/api/orders # 返回 order-api-1 或 order-api-2
curl -I -H 'Host: shop.demo.local' http://<服务器>:8001/        # Server: SyEdge
```

另开一个终端执行 `./demo.sh traffic`，总览页会出现流量曲线。

### 5. 出错了怎么办（3 分钟，本次演示的重点）

**场景 A：新后端没起来。** 新建上游 `new-api`（`172.30.0.99:9001`，一个不存在的地址），把站点的 `/` 改成反向代理到 `new-api`，发布。
看点：两台节点都在“探活”一格变红，并提示“发布后本机探活失败，已自动回滚到 r000001，流量未中断”。`curl` 仍然返回 v1。

**场景 B：证书出错进不了线上。** 先生成两套证书：`./demo.sh cert shop.demo.local` 和 `./demo.sh cert other.demo.local`。在“证书”页上传 `shop.demo.local.crt`，私钥却选 `other.demo.local.key`。
看点：控制台当场拒绝“证书与私钥不匹配”。再上传正确的一对，在站点里启用 HTTPS 并选这张证书，同时多填一个域名 `blog.demo.local`，保存被拒绝“证书不包含域名 blog.demo.local”。错误在控制台就被拦下，线上配置没有被改动。
去掉多余域名后保存并发布，在节点上验证：`docker compose exec node1 curl -skv --resolve shop.demo.local:443:127.0.0.1 https://shop.demo.local/ 2>&1 | grep 'expire date'`。

**可选：ACME 自动申请证书。** 演示服务器通常无法被 Let's Encrypt 访问，可启用内置的测试 CA（Pebble）：
`COMPOSE_PROFILES=acme ETOS_ACME_CA_FILE=/var/lib/syedge-console/acme-ca.pem ./demo.sh up`，然后执行
`docker compose cp pebble:/test/certs/pebble.minica.pem /tmp/p.pem && docker compose cp /tmp/p.pem console:/var/lib/syedge-console/acme-ca.pem`。
在“证书”页的 ACME 设置里把服务地址改为 `https://pebble:14000/dir`，点“自动申请证书”，填 `shop.demo.local`。
看点：“ACME 签发记录”依次显示 下发验证文件到节点 → 等待 CA 验证 → 签发证书 → 写入证书库并下发；“发布”页多出两条“证书续期”记录（下发验证文件、换上证书并清除验证文件）。

**场景 C：一键回滚。** 先把站点改回正常内容并发布 v2，再到“版本历史”对 r000001 点“回滚到此版本”，几秒内全部节点回到 v1。

### 6. 授权过期不影响业务（2 分钟）

1. `./demo.sh license-expired`，导入 `out/expired.lic`。
2. 顶栏变红，显示“授权已过期，控制台只读”；新建站点、发布、添加节点都被拒绝。
3. “版本历史”里回滚依然可用；`curl` 两台节点，流量完全正常。
4. 证书续期依然可用：`./demo.sh cert shop.demo.local 365` 生成新证书，在“证书”页对 shop 点“续期”。控制台以线上版本为基础只替换证书文件下发，节点核对“只有证书变化”后放行，“发布”页出现一条“证书续期”记录。
5. 演示结束重新导入 `out/demo.lic` 恢复。

讲解：授权到期只限制控制台的新增变更，绝不停止边缘流量；回滚与证书续期这类应急操作在任何授权状态下都可用。

### 7. 审计（1 分钟）

“审计”页：上面每一步都有记录（谁、何时、从哪个 IP、做了什么、结果），页头显示“哈希链校验完好”。

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `./demo.sh status` | 容器状态与访问地址 |
| `./demo.sh pull [版本]` | 拉取预构建镜像并启动 |
| `./demo.sh ps node2` | 查看节点进程 |
| `./demo.sh cert <域名> [天数]` | 生成演示用自签证书到 `out/`（默认 30 天） |
| `docker compose logs -f node1` | 查看节点 agent 日志 |
| `./demo.sh down` | 停止，保留数据 |
| `./demo.sh reset` | 清空全部数据回到初始状态 |

## 演示环境与正式部署的差异

- 节点用容器模拟，容器内没有 systemd；注册命令末尾的 `systemctl` 由一个小脚本承接，入口脚本会自动启动 agent。
- 控制台 Web 走 HTTP，正式部署应只在内网开放并配置 TLS（`-tls-cert` / `-tls-key`）。
- 授权使用演示密钥 `etos-demo-2026`，正式发布前更换为生产密钥。
