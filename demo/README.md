# EdgeTraffic OS 演示手册

在一台 Linux 服务器上，一条命令启动完整演示环境：控制台、两台边缘节点（syapp + syapp-agent）、两个演示用后端服务。全程约 15 分钟。

```
            浏览器 ──8443──▶ console（控制台）◀──9443（节点主动连接，mTLS）──┐
                                                                         │
  curl ──8001──▶ node1  edge-sg-01（syapp + agent）──┐                    │
  curl ──8002──▶ node2  edge-sg-02（syapp + agent）──┴─▶ backend1 / backend2（order-api）
```

## 准备（演示前一天完成）

| 项目 | 要求 |
| --- | --- |
| 服务器 | Linux x86_64，4 核 8 GB，Docker 24+ 与 docker compose 插件 |
| 网络 | 构建镜像时需访问 Docker Hub、GitHub（拉取上游源码）、npm |
| 端口 | 8443（控制台）、8001 / 8002（两台节点）对演示电脑开放 |
| 演示密钥 | 把 `etos-demo-2026.key` 放到 `demo/keys/`（向项目负责人索取，**不要提交到仓库**） |

```bash
git clone https://github.com/xiaoguan521/edgetraffic-os.git
cd edgetraffic-os/demo
mkdir -p keys && cp /path/to/etos-demo-2026.key keys/
./demo.sh up          # 首次构建约 5–10 分钟
./e2e.sh              # 可选：自动把整个演示跑一遍做彩排，结束后执行 ./demo.sh reset 清空
./demo.sh reset && ./demo.sh up   # 回到干净的初始状态
```

## 演示流程

### 1. 数据面是我们自己的产品（1 分钟）

```bash
./demo.sh ps node1
```

看点：`syapp version: syapp/1.30.6`；进程是 `syapp: master process` / `syapp: worker process`；整台机器上看不到 nginx 字样。
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
curl -I -H 'Host: shop.demo.local' http://<服务器>:8001/        # Server: syapp
```

另开一个终端执行 `./demo.sh traffic`，总览页会出现流量曲线。

### 5. 出错了怎么办（3 分钟，本次演示的重点）

**场景 A：新后端没起来。** 新建上游 `new-api`（`172.30.0.99:9001`，一个不存在的地址），把站点的 `/` 改成反向代理到 `new-api`，发布。
看点：两台节点都在“探活”一格变红，并提示“发布后本机探活失败，已自动回滚到 r000001，流量未中断”。`curl` 仍然返回 v1。

**场景 B：证书和私钥不匹配。** 编辑站点，启用 HTTPS，证书和私钥用不配对的两份（用 `openssl req -x509 ...` 生成两套），发布。
看点：车道在“校验”一格变红，“查看节点输出”里是 syapp 的原始报错；线上配置没有被改动。

**场景 C：一键回滚。** 先把站点改回正常内容并发布 v2，再到“版本历史”对 r000001 点“回滚到此版本”，几秒内全部节点回到 v1。

### 6. 授权过期不影响业务（2 分钟）

1. `./demo.sh license-expired`，导入 `out/expired.lic`。
2. 顶栏变红，显示“授权已过期，控制台只读”；新建站点、发布、添加节点都被拒绝。
3. “版本历史”里回滚依然可用；`curl` 两台节点，流量完全正常。
4. 演示结束重新导入 `out/demo.lic` 恢复。

讲解：授权到期只限制控制台的新增变更，绝不停止边缘流量；回滚与证书续期这类应急操作在任何授权状态下都可用。

### 7. 审计（1 分钟）

“审计”页：上面每一步都有记录（谁、何时、从哪个 IP、做了什么、结果），页头显示“哈希链校验完好”。

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `./demo.sh status` | 容器状态与访问地址 |
| `./demo.sh ps node2` | 查看节点进程 |
| `docker compose logs -f node1` | 查看节点 agent 日志 |
| `./demo.sh down` | 停止，保留数据 |
| `./demo.sh reset` | 清空全部数据回到初始状态 |

## 演示环境与正式部署的差异

- 节点用容器模拟，容器内没有 systemd；注册命令末尾的 `systemctl` 由一个小脚本承接，入口脚本会自动启动 agent。
- 控制台 Web 走 HTTP，正式部署应只在内网开放并配置 TLS（`-tls-cert` / `-tls-key`）。
- 授权使用演示密钥 `etos-demo-2026`，正式发布前更换为生产密钥。
