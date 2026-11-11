# 会话交接说明（2026-11-11）

给接手的会话看。读完这一页，再读 `CLAUDE.md` 和 `docs/dev/ops-experience-handbook.md` 第 0 节，就可以开始工作。
本页只记录“现在是什么状态、怎么干活、还剩什么”，细节以仓库里的文件为准。

## 1. 现在的状态

没有进行中的工作。下面的内容都已合并到 `main`，没有打开的 PR。

| PR | 内容 |
| --- | --- |
| #62、#63、#65–#69 | 运维体验手册的 T1–T7：文档检查门禁、快速上手页、路径约定、从 OpenResty 迁移的对照说明、独立模式一条命令启用 Lua、安装结束打印摘要、随包说明同步 |
| #64 | 原生兼容配置包的边界（`fix/legacy-migration-safety`） |
| #70 | 手册收尾：归因句规则、访问日志说明、并发升级用例、执行结果 |
| #71 | D1 第一阶段：Lua 附加包随附 10 个纯 Lua 库（redis、mysql、memcached、dns、string、upload、websocket、lock、limit-traffic、tablepool） |
| #72 | D1 第二阶段：编译库 cjson、redis.parser、rds.parser、resty.signal，纯 Lua 的 resty.upstream.healthcheck，第三个动态模块 lua-upstream（`ngx.upstream`） |
| #73 | resty.rsa（第三方，MIT；resty.mysql 不走 TLS 的 `caching_sha2_password` 完整认证需要它）。由 Codex 完成 |

负责人的结论：Lua 附加包按 `main` 上的内容定版。

## 2. 关于随附库，必须知道的几件事

- **边界**：随附库装在 `<安装根目录>/modules/lua/extlib/`，只有独立模式（`install.sh --standalone`）和原生兼容配置包能用。
  控制台托管的配置不引用 extlib、`lua_package_cpath` 保持 `?.disabled`、不加载 lua-upstream 模块，受限环境不变。
  这条边界有自动用例（`internal/render` 单元测试、`scripts/ci/lua-acceptance.sh`、`scripts/ci/lua-standalone.sh`），改动时不得放宽。
- **版本锁定**：`dataplane/lua/DEPS` 里每个库都是 GIT / TAG / COMMIT 三项；构建时校验提交哈希和许可。
- **FFI 符号**：库经 FFI 调用的 TLS 库函数必须列在 `dataplane/lua/base-exports.txt` 的 `# 附加库 extlib` 一节，构建时逐个核对基础包是否导出（标准版与国密版各一次）。宏不能导出，不要列。
- **编译库**：用 `cc` 直接编译，只依赖 LuaJIT 头文件，不链接 libluajit；Lua API 在加载时由 Lua 模块提供。构建时核对每个未定义符号都有出处。
- **做不了的**：`resty.shell` 与 `ngx.pipe`。Lua 模块只在核心带有 OpenResty 的 socket CLOEXEC 补丁时才编译这部分，而本项目的核心只打改名补丁（`CLAUDE.md` 数据面规则）。没有随附，文档已写明。
- **明确不做的**：其他 OpenResty C 模块（headers-more、echo、set-misc、srcache 等）、stream 的 Lua 模块、drizzle / postgres / iconv。负责人已决定不做。
  不在 OpenResty 发行版里的第三方库（例如 lua-resty-http）没有随附，除 resty.rsa 外没有计划。
- **升级注意**：用旧附加包启用过独立模式的节点，升级附加包后要先 `--standalone-disable` 再 `--standalone`，才会得到新的 `lua_package_path` / `lua_package_cpath` 和 lua-upstream 的加载行。

清单、版本与许可见 `docs/lua.md` 的“随附的 Lua 库”一节。

## 3. 工作方式（负责人的要求）

- **不要自己合并 PR**，由负责人合并。
- 开发分支固定为 `claude/charming-lovelace-plq1oi`。每个新任务从最新 `main` 开始：
  `git fetch origin main && git checkout -B claude/charming-lovelace-plq1oi origin/main`。已合并的 PR 不复用，新工作开新 PR。
- 推送只用 `git push -u origin claude/charming-lovelace-plq1oi`，不强推。本会话的权限只能推这个分支，不能删除其他分支。
- 负责人习惯让较小的模型做实现，主会话负责审查、修正、推送、开 PR、盯 CI。交给子代理时要说明：不切分支、不推送、不开 PR、不改写历史、
  用默认 git 身份、提交信息用英文且不写模型名。子代理提交过用例之后，实现没完成前不要推送（否则分支是红的）。
- PR 描述用手册 0.5 节的模板（任务 / 改动 / 验证 / 与手册的差异）。开 PR 后订阅它的活动，CI 红了就查原因并修。
- 先写失败用例再写实现；`make brand`、`./scripts/ci/docs-check.sh`、相关 `go test -mod=vendor` 通过后再推。
  本环境的 `make lint` 跑不了（golangci-lint 用 Go 1.25 构建，`go.mod` 要求 1.26），以 CI 为准。
- 文档和界面文案里不得出现上游 Web 服务器的名字（品牌规则，见 `scripts/brand-check.sh`）。

## 4. CI 里踩过的坑

- **构建能在本环境跑**：`dataplane/build-syedge-lua.sh -o <目录>` 约 6–7 分钟，产出两个基础包和 Lua 附加包，可以用来做运行时冒烟。
  真实数据面的用例（`lua-standalone.sh`、`lua-acceptance.sh`、`p11-acceptance.sh`）需要 sudo 和 syedge 用户，只能在 CI 上跑。
- **测试用端口不要落在临时端口范围（32768–60999）**：压测循环的源端口关闭后会保留约 60 秒，碰巧用到同一个号时数据面监听会失败。#71 里把 `p11-acceptance.sh` 的 gold 实例端口改到了范围之外。
- **aarch64 的 `__stack_chk_guard`** 由动态加载器提供；符号核对要把加载器算进去（#72 已修）。
- **`base-exports.txt` 前半部分有几个名字在某些 TLS 库版本里是宏**，所以导出核对只针对 `# 附加库 extlib` 之后的条目。
- **CI 机器偶发的网络故障**（软件源域名解析失败）会让 `accept` 里的性能验收因装不上压测工具而失败。先看日志确认，再重跑失败的任务一次；第二次还失败就当真问题查。
- 工作流还在运行时不能重跑其中的失败任务（返回 403），等它跑完。

## 5. 还没做完的事

需要负责人动手或回答的：

1. 远端的 `demo-lua-review` 分支是临时放客户脚本用的，本会话没有权限删除，需要负责人自己删。
2. 现网数据库账号用哪种认证方式还没有回答。现在 resty.rsa 已随附，不走 TLS 的 `caching_sha2_password` 也能用；ed25519 认证仍需要没有随附的 resty.openssl。
3. 手册第 5 节要求的“运维同事按快速上手页实际走一遍”还没有做。

可选的后续（都不是已决定的任务，动手前先问负责人）：

- 手册 4.1 节的 D2–D7 仍然只是建议：D2（包管理器安装 Lua）、D3、D6 建议暂缓或下一轮评估；D4、D5 建议维持现状；D7 已决定不做。
- demo 脚本的覆盖情况：29 个 Lua 文件里当前在用的 19 个，按静态检查在两个阶段之后都能覆盖。
  当时没有配套的站点配置，结论只基于脚本本身；有几处需要在真实数据面上确认（响应头键名大小写、写未声明的变量、子请求传空请求体）。
  脚本自身的问题也告诉过负责人（一处 SQL 拼接、一处把 SQL 写进日志、一处分支判断写反、一处路由键少了开头的 `/`、一个文件写死了旧安装路径）。这些脚本是客户的，不在仓库 `main` 里。

## 6. 去哪里找东西

| 想知道 | 看这里 |
| --- | --- |
| 硬性规则 | `CLAUDE.md` |
| 任务怎么做、PR 模板、本地自检命令 | `docs/dev/ops-experience-handbook.md` 第 0 节 |
| T1–T7 的结果、D1–D7 的建议 | 同一文件第 4、6 节 |
| Lua 附加包、随附库、受限环境 | `docs/lua.md` |
| 从 OpenResty 迁移时有什么、没什么 | `docs/migrate-from-openresty.md` |
| 原生兼容配置包 | `docs/legacy-config-management.md` |
| 附加包怎么构建 | `dataplane/build-syedge-lua.sh`、`dataplane/build-syedge.sh`（`LUA_MODULES_OUT` 一段）、`dataplane/lua/DEPS` |
| 独立模式启用 / 停用 | `dataplane/lua/install.sh`（`standalone_enable` / `standalone_disable`） |
| 热升级时附加包的内容校验 | `internal/wrapper/lua.go`（`luaEntryRe`） |
