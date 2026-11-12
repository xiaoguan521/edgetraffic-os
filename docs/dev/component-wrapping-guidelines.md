# 封装公共组件（Redis 等开源组件）的要求与踩坑清单

> 来源：对 `main` 上 161 个提交（2026-10-27 ～ 2026-11-12）、`CLAUDE.md`、ADR 0001-0004、`docs/lua.md`、
> `docs/upstream-upgrade.md`、`docs/dev/ops-experience-handbook.md` 与 `dataplane/lua/DEPS`、`scripts/ci/*` 的梳理。
> 每条要求后面的括号是证据（提交哈希或文件）。**本仓库当前没有任何 Redis 封装**，`CLAUDE.md` 明确"不引入 Redis / MinIO / etcd / Vault / SkyWalking"，
> 所以要封装 Redis 之前，第 0 节的"准入"必须先过。
>
> 本仓库里"封装第三方组件"实际有三类，经验各不相同：
>
> | 类别 | 代表 | 主要证据 |
> | --- | --- | --- |
> | A. Go / 前端依赖（vendor） | gin、gorm、grpc、`internal/pkg/*` | `internal/deps/tools.go`、`.github/workflows/deps.yml` |
> | B. 随附的二进制 / 原生组件（C、Lua 库） | OpenSSL/铜锁、PCRE2、LuaJIT、lua-resty-*、cjson | `dataplane/DEPS`、`dataplane/lua/DEPS`、D1 两个阶段的提交 |
> | C. 外部服务（数据库、选主） | PostgreSQL / SQLite、advisory lock | `internal/console/store`、`internal/console/ha` |
>
> Redis 属于 C 类（外部服务），同时会遇到 A 类（Go 客户端依赖）和 B 类（若要在 Lua 里用，`resty.redis`、`redis.parser` 已经随附）的约束。

---

## 0. 准入：先回答这五个问题，再动手

1. **是否被规则允许？** 引入新服务属于"需要产品决定"。`CLAUDE.md` 技术栈一节禁止 Redis；`ha` 选主选了 PostgreSQL advisory lock 而不是 Redis/etcd，
   理由是交付形态保持"一个程序加一个 PostgreSQL"（ADR 0004、`docs/ha.md`）。要用 Redis，先改 ADR 与 `CLAUDE.md`，不能在代码里先引入。
2. **许可证是否 MIT / BSD / Apache-2.0？** 新依赖只能是这三种；AGPL（nginx-ui）一行代码都不能复制（`CLAUDE.md`、ADR 0004）。
   Redis 服务端自 7.4 起不再是 BSD（RSALv2/SSPL，后续版本新增 AGPLv3 选项），**服务端是否可随产品分发需要单独评估**；Go 客户端 `go-redis`（BSD-2）与服务端许可是两件事。
3. **是否真的需要外部服务？** 先问能否用已有 PostgreSQL / 进程内实现。`secretbox`、`textdiff`、`audit` 都是自己写的小包，而不是引入组件。
4. **是否会破坏"离线可构建"？** 依赖必须能 vendor（见第 1 节），不能要求构建机联网拉取。
5. **是否会破坏"agent 只主动连接控制台、不监听端口"？** 任何需要在节点上开监听端口或常驻进程的组件，不得放在节点侧。

---

## 1. 依赖引入（A 类）：流程不能绕

| 要求 | 做法 | 证据 |
| --- | --- | --- |
| 依赖全部 vendor，构建用 `-mod=vendor` | Makefile 里 `GOFLAGS ?= -mod=vendor` | `Makefile` |
| **不要手改** `go.mod` / `go.sum` / `vendor/` / `pnpm-lock.yaml` | 在 `internal/deps/tools.go` 加 `_ "…"` 引用，推到 `deps/**` 分支，由 `deps` 工作流 tidy + vendor 后由 deps-bot 提交 | `deps.yml`、`3b9fe32`、手册 0.2 |
| proto 生成物也走同一通道 | `buf generate` 结果提交到 `api/gen/` | `deps.yml`、`909cc3a` |
| 引入前写明许可证 | `tools.go` 行内注释写许可（如 `// LDAP 登录（P9，MIT）`） | `internal/deps/tools.go` |
| 前端 node_modules 走 `deps-cache/web` 分支 | xz 分片，离线构建时解压 | `deps.yml` |
| 新增依赖必须停下来问 | 手册 0.1 第 6 条：新增 Go 模块、npm 包、Lua 库、系统软件包，或改 `*/DEPS` 都要先提问 | 手册 0.1 |
| 依赖升级不要重复下载 | CI 里"已经 vendor 就跳过更新" | `ff879da`、`3bef1aa` |

**踩坑**

- 本地没有网络时，`go mod tidy` 会把还没被引用的依赖删掉：先在 `tools.go` 里引用，再 tidy（该文件注释："真正使用后可从此处删除"）。
- 只改 `go.mod` 不跑 vendor，CI 的 `-mod=vendor` 会报 inconsistent vendoring。

---

## 2. 随附原生组件（B 类）：D1 两个阶段沉淀下来的硬要求

这是历史里踩坑最集中的地方（`c4d80f3`→`98df720`→`d6f7a0f`→`6136b91`→`0afb207`→`a8cf883`→`1945606`→`ab139d5`）。

### 2.1 锁定与校验

- **标签 + 完整提交哈希同时锁定**，克隆后 `rev-parse HEAD` 校验；升级时两者一起改（`dataplane/lua/DEPS` 文件头）。
  对带注解的标签要钉"剥离后的提交"（`6136b91` 提到 lua-resty-shell 是 annotated tag，peeled commit）。
- 静态依赖用版本 + URL + **sha256**，下载后逐一校验（`dataplane/DEPS`）。
- 版本配套关系写进 DEPS 注释：lua-resty-core v0.1.32R1 要求 lua 模块 0.10.29（`DEPS` 第 3 行）。
- 只取正式发布标签，不取 rc（`DEPS` 注释）。
- 版本跟踪自动化：`scripts/ci/check-upstream.sh` + `upstream-check.yml` 每周一次。**同一版本线内比较**（OpenSSL 3.5.x 而不是 3.6），
  更新的大版本线只提示不失败；取不到 tag 时退出码 2，而不是当成"已最新"（`e075065`）。
  踩坑：`git ls-remote --tags` 会列出 `^{}` 的 peeled 条目，要加 `--refs`，否则永远显示"有新版本"。

### 2.2 许可与归因

- 构建时**机器校验**许可：从各库 README 提取 "Copyright and License"，校验 BSD 声明（接受 "licenced" 拼写，`6136b91`）；MIT 库校验 LICENSE 全文。
- 每个组件的名称、版本、哈希、版权行、许可条文写入 `docs/NOTICE`，并随包的 `NOTICE` 一起分发（`docs/NOTICE`）。
- **不随附的组件要同步从 DEPS、NOTICE、文档、CI 断言里删干净**（`0afb207` 同时改了 7 个文件）。
- 第三方库不改源码，只拷贝需要的文件（"copy .lua only"）；与已有模块重名必须在构建时报错（"reject module-name collisions with lualib"，`98df720`）。

### 2.3 隔离边界：新能力默认不进托管路径

- 随附库放在独立目录（`extlib/`），**不进入托管渲染的搜索路径**；只对独立模式和原生兼容配置包可用（`CLAUDE.md` 数据面一节）。
- 这条边界要有**两层自动用例**：`internal/render` 的单元测试断言渲染结果不含 `extlib`；`lua-acceptance.sh` 断言托管脚本 `require "resty.redis"` 必须失败（`docs/lua.md` 第 188 行）。
  → **封装 Redis 时同理**：托管（managed）路径是否允许访问 Redis，必须有明确结论和反向用例。
- 任何放宽受限环境的改动，必须在验收脚本里有对应用例（`CLAUDE.md`）。

### 2.4 能不能用，先在目标内核上验证（别发"装了但加载不了"的东西）

- `lua-resty-shell` 的后端 `ngx.pipe` 只有核心带 socket CLOEXEC 补丁才会编译进模块，而 syedge 只打改名 patch → 该库"装得上、加载不了"，最终撤掉并加断言"require 必须报找不到模块"（`0afb207`）。
  **教训：一个组件的依赖链里只要有一环要改核心，就等于不能封装；要先做最小可运行验证，再写封装代码。**
- 第三方库依赖的库也要查：`resty.mysql` 的 ed25519 认证需要 `resty.openssl`，未随附，要在文档写成"限制"而不是留给用户踩（`docs/lua.md`）。

### 2.5 符号与链接

- 静态链接的主程序只包含核心用到的 OpenSSL/PCRE2 目标文件。FFI 库需要的额外符号必须在 `dataplane/lua/base-exports.txt` 登记，由 `-Wl,-u` 强制链接并导出；构建时用 `nm` 核对"附加模块每个未定义强符号都由基础包提供"。
  新增依赖时据此补充（`98df720`）。
- 编译型 C 库：只依赖 LuaJIT 头文件，用 `cc` 直接编译，**不链接 libluajit**（`DEPS`、`docs/lua.md`）。
- 校验未定义符号时要处理平台差异：
  - 一部分符号在某些 TLS 库版本里是**宏**，不会出现在动态符号表里 → 只检查"随附库"那一段，不检查整个 base-exports（`a8cf883`）。
  - aarch64 的 stack protector guard 符号在动态加载器里，`ldd` 打印时没有箭头 → 要把加载器的导出也算进去（`1945606`）。
  → **所有"符号 / 链接"类检查在 amd64 与 arm64 都要跑**（`lua` 工作流矩阵：amd64/arm64 × 标准/国密）。

### 2.6 打包与升级通道

- 新增文件类型要同步放行：wrapper 热升级包的白名单要接受 `addon/extlib/**/*.so` 与 `ngx_http_lua_upstream_module.so`，同时**仍拒绝形似路径与目录穿越**（`d6f7a0f`、`internal/wrapper/lua_test.go`）。
- 升级包必须带厂商 Ed25519 签名，由 wrapper 验签（`CLAUDE.md`）。
- 已安装旧版附加包的节点，新版路径不会自动生效，需要 `--standalone-disable` 再 `--standalone`（`docs/lua.md` 第 182 行）。**新增路径/配置项时要写明旧节点的升级动作**，或者实现自动刷新。

---

## 3. 外部服务（C 类）：Redis 最该参考的两个先例

### 3.1 先例一：数据库（`internal/console/store`）

- **访问集中在一个包里**（MVP 在 `store`，正式版迁 gorm/gen）；所有写接口写审计；只用参数化查询（`CLAUDE.md`）。
- **同时支持 SQLite 与 PostgreSQL**：`store.Open` 按 DSN 前缀分流；SQLite 固定 `busy_timeout(5000)`、WAL、`foreign_keys(1)`。
  → Redis 若被引入，同样要有"不可用 / 未配置"的降级路径，而不是假设它一直在。
- 生成代码必须同步：`make gen-check` 在 CI 中失败即说明模型与生成物不一致。
- 方言差异会在 CI 暴露：`b8ca80e` 修"PostgreSQL 上索引列顺序不稳定"，`bd81b9a` 修"PostgreSQL 审计断言"。
  → **所有存储层测试要能用 `ETOS_TEST_DB` 切到真实后端跑**（`make test-imports`），不要只在内存/SQLite 上通过。

### 3.2 先例二：选主（`internal/console/ha`）

- 用 PostgreSQL **会话级 advisory lock**，键是固定常量（"ETOSHA01" 的 ASCII）；**只要使用 PostgreSQL 就总是参与选主**，所以误启动的第二台不会同时下发；无 `ha` 授权时备机不自动接管（`docs/ha.md`）。
- 有故障切换的实测脚本和门槛（`scripts/ci/ha-failover.sh`，26 秒 + VIP 4 秒 < 30 秒）。
  → 若用 Redis 做锁/选主：需要说明为什么不是 advisory lock；必须覆盖"网络分区、进程被 SIGSTOP、连接闲置被回收"，并给出实测门槛。

---

## 4. 通用封装要求（对所有组件适用）

### 4.1 接口与目录

1. 公共封装放 `internal/pkg/<名字>/`，包注释说明**用途、边界、不做什么**（参考 `secretbox`：说明密钥文件权限 0600、不进数据库/日志/API 响应）。
2. **对外只暴露本产品需要的最小接口**，不透传第三方类型。业务代码不得直接 import 第三方包（`internal/pkg/*` 之外）。
3. 业务代码只通过 `license.Evaluate` 的 Snapshot 读授权，类似地，业务代码只通过封装层读组件状态；需授权的能力挂 `RequireFeature` / `RequireQuota` / `RequireWritable`，前端置灰不算约束。
4. 配置、凭据、连接串**不得出现在日志、代码、前端响应中**；格式化错误时脱敏（`CLAUDE.md` 私钥一节；`secretbox.ID()` 只暴露单向哈希前 8 字节，值得照搬）。
5. 加密/落盘格式带版本前缀（`secretbox` 的 `"v1:"`），为以后换算法留口子。

### 4.2 命令与进程

- 所有外部命令用 `exec.Command` 切片，**禁止 `sh -c` / `eval`**；用户输入不得拼进命令或配置（`CLAUDE.md`，手册 0.2）。
- 子命令白名单 + 正则校验（`test` 只接受 `^r[0-9]{6,12}$`）；**每个子命令都要有注入测试**。
- 不得新增 agent 指令：agent 协议里没有"执行命令 / 读写任意文件"类指令，若组件需要节点侧动作，只能复用现有两个受限消息或新增专用且受限的消息（要改 `api/proto/`，必须先问）。
- 不得为生效而 restart 数据面：只能 reload（HUP）（`CLAUDE.md`）。

### 4.3 配置生成

- 一律经 `internal/render` 的校验器 + 模板 + 转义；Lua 代码只来自审核过的模板。
- 每次改模板，渲染黄金用例必须通过真实 `syedge -t`。**但 `-t` 不会执行 `init_by_lua`**：语法错误要到 reload 才暴露，agent 要识别并回滚（`18a5fef`）。
  → 封装的校验链路不能只依赖"干跑"：要有真实 reload 的验收用例，并确认 agent 的失败识别（`badLog`）覆盖新错误。

### 4.4 品牌门禁

- 产品代码、界面、日志、打包脚本中不得出现 "nginx"（`make brand`）。引入第三方库时，**库自己的名称含上游名**（如 `gonginx`、`lua-nginx-module`）要放进白名单或限定在 `dataplane/lua/DEPS` 与 `docs/`、`scripts/ci/`，不能漏到 `cmd/ internal/ web/src/ deploy/`。
- 归因句 "Based on NGINX Open Source (BSD-2-Clause)" 必须保留（大小写不敏感，`9f2abc7`）。

---

## 5. 测试与 CI：踩过的坑

| 坑 | 现象 | 对策 | 证据 |
| --- | --- | --- | --- |
| 测试端口落在临时端口段 | 压测后源端口占 TIME_WAIT，新实例绑定失败 | 测试端口放在临时端口段**以下**；失败时打印 unit journal | `4080fb5` |
| 固定端口被占 | `Address already in use` | 让内核分配空闲端口，并等占用进程就绪后再检查 | `defbdf1`、`087c716` |
| 墙钟断言 | 繁忙 CI 机器上并发测试超时 | 只排除"完全串行"，并发性用计数器（`fa.max`）断言 | `4136e29` |
| arm64 runner TLS 不稳 | 上传 release asset 失败 | 构建作业用 workflow artifact 交接，上传加长重试 | `663bc5c`、`dc5d83f` |
| 基准工具安装失败拖垮功能检查 | 压测准备失败后功能用例没跑 | 功能验收先于压测安装；压测失败也要跑功能检查 | `cb3e374`、`ace9b7d` |
| 压测门槛只在 x86 | arm64 抖动 | QPS 门禁只在 x86_64 阻断，arm64 仅报告 | `bc829ea` |
| 真实环境用例被跳过 | 以为通过实际没跑 | 真实数据面用例**不得 skip**（`SYEDGE_LEGACY_IT_REQUIRED`），lua 工作流中缺环境就失败 | `6fc5d7f`、`CLAUDE.md` |
| 机器唯一标识为空 | 两个容器节点指纹相同被判冲突 | 无 `/etc/machine-id` 时不生成指纹，控制台忽略空 ID 哈希 | `5400b06` |
| 吊销后资源未释放 | 重新添加被 `HOST_CONFLICT` | 释放占用的唯一约束 | `17eab99` |
| lint | `error strings must not start with a capital letter (ST1005)` | 错误信息小写开头 | `4f27082` |

**节奏要求**（`CLAUDE.md` 交付要求 + 手册 0.1）：

1. 先提交**会失败的检查**（红），再提交实现（绿）。历史里 D1 就是 `c4d80f3`(红) → `98df720`(绿)，`d6f7a0f`(红) → `6136b91`(绿)。
2. 一个任务一个分支一个 PR，不顺手重构。
3. 每个 PR 附可运行的测试命令；`make test`、`make lint`、`make brand` 通过才可提交。
4. 同一个检查连续三次修不好就停下，在 PR 里写清现象。
5. 禁止通过 `t.Skip`、删除测试、注释 CI 步骤来换通过。
6. 文档也有门禁：`scripts/ci/docs-check.sh` 检查链接存在，并按 `docs-required.txt` 检查关键字符串；新增能力要同步往 `docs-required.txt` 加条目。

---

## 6. 兼容性与运行环境（影响所有"装在节点上"的东西）

- **不使用 root 运行**：master/worker/wrapper 以 `syedge` 运行，只带 `CAP_NET_BIND_SERVICE`；防火墙加载只带 `CAP_NET_ADMIN`（`0af6739`）。
  组件如需额外权限，必须单独走 systemd 单元的 capability，不得顺手用 root。
- **老系统（CentOS 7、systemd 208/219）会让很多"标准写法"失效**（`e53d1ea`、`898ddbf`、`cff6585`）：
  - `systemctl enable --now` 需要 systemd 220 → 拆成 `enable` 与 `start`；
  - `RuntimeDirectory=` 需要 211，且 `/run` 开机清空 → 写 `/etc/tmpfiles.d`；
  - 229 之前多行 `CapabilityBoundingSet=` 是取交集 → 只生成单行的完整覆盖单元；
  - sudo 默认 `requiretty` → 加 `Defaults:<agent> !requiretty`；
  - 内核不支持 ambient capability → 兼容模式（root master，仅此一种例外，由 root 所有的 `/etc/syedge/root-master` 标记决定）。
- **前缀不是固定的**：安装根目录可自定义（`/etc/syedge/prefix`），`include` 一律相对路径，版本目录自包含；文档和脚本里不要写死 `/opt/syedge`（手册 §1 第 2 条，`12868c4`、`dac70f7`…`2e084ec`）。
- 最小系统要声明依赖（`runuser`，`0a9865a`）；平台相关代码要加构建标签（`70bbfb3`：磁盘告警只在 Unix 构建，否则 Windows release 构建失败）。
- 打包产物要保持所有权和权限（`d09fe76`、`b165ad9`）；备份文件要保护（`b165ad9`）。

---

## 7. 异常边界要写成"例外条款"，并配自动用例

仓库对"不得不开的口子"的处理方式值得照搬（`CLAUDE.md` 的"自定义 Lua"、"原生兼容配置包"两条）：

1. 在 `CLAUDE.md` 写**决定日期、适用范围、谁能用**（管理员提交 + 另一位管理员批准，批准绑定冻结包摘要）；
2. 写明**默认拒绝**（master 为 root 的节点默认拒绝，必须声明并审批并写审计）；
3. 写明**上限**（压缩后/解压后/文件数）与**只读最小内容**（只接受普通文件，无软链接/特殊文件）；
4. 日志、审计、比较里**只出现路径和哈希，不出现内容**；私钥文件只给哈希；
5. **每一条边界都有自动用例**，且真实环境的用例不得被跳过。

封装 Redis 若要开放用户可写的能力（例如允许用户自定义 key 前缀、Lua 脚本），按这五条做。

---

## 8. 封装一个新组件的检查清单（可直接复制到 PR 描述）

```markdown
## 准入
- [ ] 已在 ADR / CLAUDE.md 记录"为什么要引入"，并通过产品决定（Redis 原本被禁止）
- [ ] 许可证为 MIT / BSD / Apache-2.0；若是服务端组件，已评估随产品分发的许可风险
- [ ] 已确认不需要修改 syedge 核心或改名 patch（依赖链上没有"需要核心补丁"的一环）
- [ ] 不会让 agent 监听端口，不增加"执行命令 / 读写文件"类协议指令

## 依赖
- [ ] 通过 internal/deps/tools.go + deps/** 分支引入，未手改 go.mod / vendor / lockfile
- [ ] 原生组件：标签 + 提交哈希（或 sha256）锁定，克隆后校验
- [ ] 构建时校验许可声明；docs/NOTICE 与随包 NOTICE 已更新
- [ ] 已加入 check-upstream.sh 的版本跟踪（若是上游组件）

## 封装
- [ ] 放在 internal/pkg/<名字>/，业务代码不直接 import 第三方包
- [ ] 对外接口最小；凭据 / 连接串不进日志、API 响应、代码
- [ ] 外部命令用切片，无 sh -c；输入有正则 / 白名单校验，并有注入测试
- [ ] 写接口写审计；需授权的能力挂 RequireFeature / RequireQuota / RequireWritable
- [ ] 不可用 / 未配置 / 超时 有降级路径；EXPIRED/INVALID 下的必须可用项不受影响
- [ ] 托管（managed）路径是否可用已有明确结论，并有反向用例

## 测试
- [ ] 先提交失败用例（红），再提交实现（绿）
- [ ] 真实后端用例可用环境变量切换，CI 中真实环境用例不会被 skip
- [ ] amd64 与 arm64 都跑（涉及符号 / 链接 / 原生库时）
- [ ] 测试端口不在临时端口段；不使用固定被占端口；不依赖墙钟时间
- [ ] make test、make lint、make brand 通过；docs-required.txt 已加条目

## 兼容
- [ ] 在 CentOS 7 / systemd 208 路径上验证过（或在文档中明确不支持）
- [ ] 不写死 /opt/syedge；路径经 internal/layout
- [ ] 旧节点升级动作已写明（或已自动处理）
```

---

## 附：Redis 专项提醒（基于上述经验的推断，非历史已有结论）

- 当前仓库里和 Redis 相关的内容只有 Lua 侧随附库 `lua-resty-redis`、`lua-redis-parser`（BSD-2-Clause），它们是**数据面里访问用户自己的 Redis**，与控制台是否依赖 Redis 无关，别混淆。
- 若控制台侧要用 Redis（缓存、会话、锁）：先回答第 0 节；其次选用 BSD/MIT/Apache 的 Go 客户端，经 deps 通道引入；封装在 `internal/pkg/<名字>/`，对外只暴露本产品用到的 Get/Set/Lock 等最小接口；
  DSN 与密码走 `secretbox` 加密保存、日志脱敏；必须有"未配置 / 不可用"的降级；测试要同时覆盖真实 Redis 与不可用情形，并在 CI 中通过服务容器提供真实后端，**不得 skip**。
- 若要把 Redis 作为产品交付物（随包分发服务端）：除许可评估外，还要考虑不使用 root 运行、systemd 老版本兼容、打包所有权、`NOTICE`、升级与备份——这些都在上面各节出现过。
