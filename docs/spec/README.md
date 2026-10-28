# 设计规格书

- 主文档（在线，可评论）：[EdgeTraffic OS 设计规格书 v3.1](https://claude.ai/artifact/6nhk8LuTBm3hqjBD1MryCp)
- 章节与代码对应：

| 章节 | 代码 |
| --- | --- |
| 4 数据面 syapp | `dataplane/`、`scripts/rename-scan.sh`、`scripts/golden-rename.sh` |
| 5 syapp-agent | `cmd/syapp-agent`、`internal/agent/*`（漂移检测见 `apply.TreeDigest`）、`internal/wrapper`、`api/proto/agent/v1` |
| 6 配置模型与渲染器 | `internal/render` |
| 7 控制台功能 | `internal/console/*`、`web/` |
| 8 商业授权 | `internal/license`、`cmd/syapp-license-server`、`license-portal/` |
| 9 安全设计 | `dataplane/packaging/{sudoers,nftables,systemd}`、`internal/console/pki` |
| 10 数据模型与 API | `internal/model`、`internal/migrate`、`api/openapi` |
| 12 分期路线 | 见下表 |

| 阶段 | 状态 |
| --- | --- |
| P0 数据面 | 完成：1.30.6 改名构建，黄金验收通过 |
| P1 渲染器 | MVP 完成：模型、校验、模板、确定性打包，真实 syapp -t 验证 |
| P2 agent | MVP 完成：注册、mTLS 长连接、发布状态机、自动回滚、漂移检测（心跳校验配置目录与清单，控制台标记“配置被手动改动”）、指标采集；漂移一键恢复待实现 |
| P3 控制台后端 | MVP 完成：API、RBAC、发布编排、回滚、自动对齐、审计哈希链；SQLite / PostgreSQL |
| P4 商业授权 | MVP 完成：验签、状态机、门禁中间件、agent 侧校验、签发 CLI；授权后台 Web 待实现 |
| P5 控制台前端 | MVP 完成：初始化、登录、总览、节点、站点与上游、发布、审计、授权 |
| 构建与发布 | 完成：多架构镜像（GHCR，amd64 / arm64）、控制台多平台二进制、节点安装包与 install.sh，`v*` 标签出正式 Release |

当前基线版本：`v0.1.0`（MVP 演示版）。

正式版路线（规格书 12.2–12.8，2026-10-28 定稿）：P6 正式版基础 → P7 配置导入与审批 → P8 统一升级与国密 → P9 商业功能 → P10 Lua 扩展，计划 2027 年 3 月发布 v1.0。12.8 的 6 项决策已同步进 `CLAUDE.md`。

演示环境与演示手册：[`demo/README.md`](../../demo/README.md)。
