# EdgeTraffic OS —— 编码规则（违反任一条即拒绝合并）

完整设计见 docs/spec/README.md 中的规格书链接。每个任务只引用规格书对应章节。

## 技术栈
- 后端 Go + Gin + GORM/gen + gormigrate + grpc；前端 Vue 3 + TS + Vite + antdv-next + Pinia。
- 不引入 Redis / MinIO / etcd / Vault / SkyWalking。新增依赖必须是 MIT / BSD / Apache-2.0 许可。
- 不得复制 nginx-ui（AGPL-3.0）的任何代码。

## 数据面
- syapp = nginx 官方 stable 源码 + dataplane/patches/0001-syapp-rename.patch。
  patch 只允许改字符串字面量与宏的值，禁止改动判断、结构体、函数签名、模块 ABI。
- 信号：reload=HUP，quit=QUIT，stop=TERM，reopen=USR1，USR2 仅用于二进制热升级。
- 永远不得 restart syapp；任何代码路径（含授权失效）不得停止 syapp。

## 配置
- 禁止用字符串拼接用户输入生成配置；一律经 internal/render 的校验器 + 模板 + 转义函数。
- 版本目录自包含，include 一律相对路径；--conf-path=/opt/syapp/conf/current/syapp.conf。
- location 内禁止 add_header；location 内出现 proxy_set_header 时必须重发基线 5 条。
- 每次修改模板都要让 render 黄金用例通过真实 `syapp -t`。

## agent 与安全
- agent 只主动连接控制台，不得监听任何端口。
- agent 协议中不得加入「执行命令」「读写任意文件」类指令。
- 所有外部命令用 exec.Command 切片调用，禁止 sh -c。
- wrapper 只接受子命令 test|reload|reopen|quit，test 只接受 ^r[0-9]{6,12}$。
- 私钥、凭据、License 签名私钥不得出现在代码、日志、前端响应中。
- 数据库访问只用参数化查询；MVP 阶段集中在 internal/console/store 封装，正式版迁移到 gorm/gen。每个写接口写审计。

## 授权
- 业务代码只通过 license.Evaluate 产出的 Snapshot 读取授权，不得自行解析 .lic。
- 需授权的 API 必须挂 RequireFeature / RequireQuota / RequireWritable 中间件；前端置灰不算约束。
- EXPIRED/INVALID 状态下回滚、证书续期、导入 License 必须仍可用。

## 品牌
- 产品代码、界面文案、日志、打包脚本中不得出现 "nginx"（范围见 scripts/brand-check.sh）。
- 关于页与 docs/NOTICE 必须保留 "Based on NGINX Open Source (BSD-2-Clause)"。

## 依赖
- 依赖全部 vendor 在仓库中：新增依赖时在 internal/deps/tools.go 引用并推送到 deps/** 分支，由 deps 工作流 tidy + vendor 后提交。
- 前端 node_modules 缓存在 deps-cache/web 分支（xz 分片），离线构建时解压到 web/。

## 交付要求
- 先写失败用例再写实现；每个 PR 附可运行的测试命令。
- make test、make lint、make brand 全部通过才可提交。
