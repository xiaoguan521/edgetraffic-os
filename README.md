# SyEdge（神玥企业级动态网关与负载均衡引擎）

基于 NGINX 开源版的边缘流量管理产品：数据面 **syedge** 只做改名构建、不改逻辑；
控制面是自研 Web 控制台，统一管理多台边缘节点的「改配置 → 校验 → 下发 → reload → 验证 → 回滚」，并内置商业授权。

- 设计规格书：[SyEdge 设计规格书 v3.1](https://claude.ai/artifact/6nhk8LuTBm3hqjBD1MryCp)（副本索引见 `docs/spec/`）
- 安装维护手册：[`docs/install-maintenance.md`](docs/install-maintenance.md)
- 编码规则：[`CLAUDE.md`](CLAUDE.md)（所有编码 Agent 与开发者必读）

## 组件

| 程序 | 位置 | 职责 |
| --- | --- | --- |
| `syedge-console` | 客户内网 | Web 控制台、REST API、渲染器、发布编排、内置 CA、审计、License 校验、gRPC 网关 |
| `syedge-agent` | 每台边缘节点 | 主动连接控制台，本地发布状态机、指标采集、漂移检测；不监听端口 |
| `syedge-wrapper` | 每台边缘节点 | agent 唯一的提权入口，只执行 test / reload / reopen / quit |
| `syedge` | 每台边缘节点 | 数据面：处理公网流量 |
| `syedge-license-server` | 厂商内网 | 客户与订单、License 签发、激活、吊销 |

## 目录

```
cmd/              四个 Go 程序入口
api/proto/        控制台 <-> agent gRPC 协议
internal/         console / agent / render / license / wrapper / model / query / migrate / pkg
web/              控制台前端（Vue 3 + Vite + antdv-next）
license-portal/   授权后台前端
dataplane/        上游锁定、改名 patch、构建脚本、打包文件
deploy/           安装脚本、docker-compose、systemd
scripts/          品牌门禁、改名扫描、改名黄金验收
test/             e2e 与前端权限矩阵
docs/             NOTICE、ADR、规格书索引
```

## 开发

```bash
make test        # go vet + go test
make build       # 构建四个 Go 程序到 bin/
make brand       # 品牌门禁
make web         # 前端构建（需 pnpm）
```

## 当前进度

MVP（演示版）已完成，见 [`docs/spec/README.md`](docs/spec/README.md)。一键演示环境与演示手册见 [`demo/README.md`](demo/README.md)：

```bash
cd demo && ./demo.sh up
```

## 许可

本仓库为专有软件，见 [LICENSE](LICENSE)。数据面基于 NGINX Open Source（BSD-2-Clause），归因见 [docs/NOTICE](docs/NOTICE)。
