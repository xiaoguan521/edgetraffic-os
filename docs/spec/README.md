# 设计规格书

- 主文档（在线，可评论）：[EdgeTraffic OS 设计规格书 v3.1](https://claude.ai/artifact/6nhk8LuTBm3hqjBD1MryCp)
- 章节与代码对应：

| 章节 | 代码 |
| --- | --- |
| 4 数据面 syapp | `dataplane/`、`scripts/rename-scan.sh`、`scripts/golden-rename.sh` |
| 5 syapp-agent | `cmd/syapp-agent`、`internal/agent/*`、`internal/wrapper`、`api/proto/agent/v1` |
| 6 配置模型与渲染器 | `internal/render` |
| 7 控制台功能 | `internal/console/*`、`web/` |
| 8 商业授权 | `internal/license`、`cmd/syapp-license-server`、`license-portal/` |
| 9 安全设计 | `dataplane/packaging/{sudoers,nftables,systemd}`、`internal/console/pki` |
| 10 数据模型与 API | `internal/model`、`internal/migrate`、`api/openapi` |
| 12 分期路线 | 见下表 |

| 阶段 | 状态 |
| --- | --- |
| P0 数据面 | 未开始（构建脚本、打包文件已就位，patch 待生成） |
| P1 渲染器 | 校验器已实现；模板与模型待实现 |
| P2 agent | wrapper 已实现；其余待实现 |
| P3 控制台后端 | 未开始 |
| P4 商业授权 | 验签与状态机已实现；中间件、授权后台待实现 |
| P5 控制台前端 | 路由与页面占位 |
