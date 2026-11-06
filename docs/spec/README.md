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
| P2 agent | MVP 完成：注册、mTLS 长连接、发布状态机、自动回滚、漂移检测（心跳校验配置目录与清单，控制台标记“配置被手动改动”）、指标采集（漂移一键恢复已在 P6 完成） |
| P3 控制台后端 | MVP 完成：API、RBAC、发布编排、回滚、自动对齐、审计哈希链；SQLite / PostgreSQL |
| P4 商业授权 | MVP 完成：验签、状态机、门禁中间件、agent 侧校验、签发 CLI |
| P5 控制台前端 | MVP 完成：初始化、登录、总览、节点、站点与上游、发布、审计、授权 |
| 构建与发布 | 完成：多架构镜像（GHCR，amd64 / arm64）、控制台多平台二进制、节点安装包与 install.sh，`v*` 标签出正式 Release |
| P6 正式版基础 | 完成（除正式密钥仪式）：证书库与 ACME、RPM / DEB 与静态 syapp、nftables、gorm/gen + 冻结迁移、控制台 HTTPS、PostgreSQL、加密备份恢复、漂移一键恢复、独立模式、离线签名工具与授权后台（`syapp-license-server serve`）。正式 License / 升级包签名密钥待离线仪式，开发期沿用演示密钥 |
| P7 配置迁移与人工配置治理 | 完成：发布审批（双人复核）、自定义片段（三个插入点白名单 + 强制另一人批准）、配置导入（-T / tar.gz → 站点模型，逐行报告）、漂移 diff 上报与采纳 |
| P8 数据面统一升级 | 完成：升级包厂商签名（etos-pkg 密钥）、wrapper 热升级子命令（真实 syapp 演练通过）、离线签名工具 sign-package、控制台版本库与分批编排、agent 执行与自升级、国密变体（铜锁 NTLS 构建 + 国密 TLCP 双证书，syapp 源码与改名 patch 不变，见 `docs/gm.md`） |
| P9 商业功能 | 完成：开放 API 令牌与 OpenAPI、告警（邮件 / Webhook / 企业微信 / 钉钉）、发布审批、灰度（5xx 自动回滚）与批量发布、等保 2.0 账号安全、LDAP / OIDC 单点登录、四层代理（TCP / UDP / SNI）、静态资源发布（原子切换）、控制台主备高可用（PostgreSQL 选主 + keepalived，见 `docs/ha.md`） |
| P10 访问规则与 Lua 扩展 | 完成：原生规则积木（IP / IP 集与地域库 / 请求头 / UA / 路径 / 方法 → 拒绝 / 跳转 / 限速 / auth_request 外部鉴权）；独立的 `syapp-lua` 附加包（OpenResty 的 LuaJIT 2.1、ngx_devel_kit、lua-nginx-module、lua-resty-core / lrucache，版本与提交锁定，动态模块装在 `/opt/syapp/modules/`，兼容标准版与国密版，syapp 核心与改名 patch 不变）；JWT HS256 / RS256 模板规则与沙箱；授权 `lua` + 节点附加包发布门禁；带附加包的热升级与回退；amd64 / arm64 × 标准 / 国密 CI 验收（见 `docs/lua.md`） |

| P11 部署布局 | 完成（PR #30–#32）：同机多实例（`syapp-instance add/remove/list/check/reopen-all`，`syapp@` / `syapp-agent@` 单元，实例间隔离与热升级）、user 模式（无 root 安装）、运行时不使用 root（D5：system 模式 master / worker / wrapper 以 syapp 运行，只带 CAP_NET_BIND_SERVICE；防火墙以 syapp-firewall 运行；`syapp-instance nonroot` 无中断切换旧 root master）、品牌可定制（D7：Server 头默认 `syapp`，自定义品牌如 `SyEdge`，HTTP/1.1 与 HTTP/2 一致）；CI 验收见 `scripts/ci/p11-acceptance.sh`、`scripts/ci/brand-runtime.sh`，见 [`docs/design/0001-deploy-layout.md`](../design/0001-deploy-layout.md) |

| 验收补齐 | 规格书量化验收全部自动化（见 `docs/acceptance.md`）：50 节点发布 / 控制台宕机 30 分钟 / 分批热升级与回退（`scale`）、主备切换时间与 v0.1.0 升级（`ci`）、前端角色矩阵与实时刷新（`demo-e2e`）。仍需线下完成：正式签名密钥仪式、麒麟 V10 / 统信 UOS 安装冒烟、10 份真实客户配置导入 |

当前基线版本：`v0.1.0`（MVP 演示版）；P6、P7 完成后计划发 v0.2 给试点客户。

正式版路线（规格书 12.2–12.8，2026-10-28 定稿）：P6 正式版基础 → P7 配置导入与审批 → P8 统一升级与国密 → P9 商业功能 → P10 Lua 扩展，计划 2027 年 3 月发布 v1.0。12.8 的 6 项决策已同步进 `CLAUDE.md`。

演示环境与演示手册：[`demo/README.md`](../../demo/README.md)。
