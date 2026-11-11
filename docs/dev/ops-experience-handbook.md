# 运维体验改进 —— 开发手册

本手册面向执行任务的编码 Agent（以及人工开发者）。目标是缩小 SyEdge 与 OpenResty 在“安装”和“对接使用”上的手感差距。
每个任务都是一个独立的 PR，由 GitHub Actions 验证。**一次只做一个任务，按编号顺序做。**

- 编码规则以仓库根目录 [`CLAUDE.md`](../../CLAUDE.md) 为准，本手册不重复全部规则，只列最容易踩到的。
- 手册中的文件路径、行为描述以 2026-11-11 的 `main` 为准。动手前先读一遍任务“涉及文件”里列出的文件；
  若发现与手册描述不一致，以代码为准，并在 PR 描述的“与手册的差异”一节写明。

## 0. 执行规则（必读）

### 0.1 工作方式

1. 一个任务 = 一个分支 = 一个 PR。分支名 `ops-ux/t<编号>-<短名>`，例如 `ops-ux/t1-docs-check`。从最新的 `main` 拉分支。
2. 不要把两个任务合进一个 PR；不要顺手重构、改名、调整格式。PR 的改动只限该任务“涉及文件”一节列出的文件，
   确需改动其他文件时，在 PR 描述中逐个说明原因。
3. 先写会失败的检查（测试用例、验收脚本断言、`docs-required.txt` 条目），提交一次；再写实现，再提交。
4. 推送前在本地执行任务中“本地自检”列出的命令。本地没有 `golangci-lint`、`pnpm`、Docker 时跳过对应命令，交给 CI。
5. 开 PR 后等待 Actions。**全部必需的检查通过才算完成**（见 0.3）。失败时读日志、修复、再推送；
   同一个检查连续三次修不好就停下，在 PR 里写清楚现象和已尝试的做法，等人处理。
6. 遇到下列情况立即停下并在 PR / 任务说明中提问，不要自行决定：
   - 需要新增第三方依赖（Go 模块、npm 包、Lua 库、系统软件包）；
   - 需要改动 `dataplane/patches/`、`dataplane/UPSTREAM`、`dataplane/DEPS`、`dataplane/lua/DEPS`；
   - 需要改动 `internal/wrapper/`、sudoers、systemd 单元的权限相关字段；
   - 需要改动授权（`internal/license/`）或 agent 协议（`api/proto/`）；
   - 任务的验收标准与现有测试冲突。

### 0.2 绝对不能做的事

| 禁止 | 原因 |
| --- | --- |
| 跳过、删除、`t.Skip`、注释掉现有测试或 CI 步骤来换取通过 | 违反交付要求 |
| 在 `cmd/`、`internal/`、`web/src/`、`deploy/`、`dataplane/packaging/`、`dataplane/lua/install.sh`、`dataplane/lua/uninstall.sh`、`dataplane/lua/lualib/` 中写上游名（见 `scripts/brand-check.sh` 的 `SCOPE`） | 品牌门禁；`docs/` 与 `scripts/ci/` 不在检查范围内，可以写 |
| 让任何脚本 `restart` 或停止 `syedge` | 只允许 reload（HUP） |
| 用 `sh -c`、`eval`、`bash -c "$变量"` 执行拼出来的命令 | 安全规则 |
| 把用户输入拼进配置文件 | 配置只能由固定文本或 `internal/render` 生成 |
| 修改 `vendor/`、`go.mod`、`go.sum`、`web/pnpm-lock.yaml` | 依赖只由 deps 工作流更新 |
| 在提交、PR、代码注释中写入密钥、令牌、个人邮箱 | 安全规则 |

### 0.3 PR 触发哪些 Actions

| 工作流 | 触发条件（`pull_request`） | 内容 |
| --- | --- | --- |
| `ci` | 任何 PR | `go`（gofmt、golangci-lint、`make test`、构建）、`postgres`、`firewall`、`brand`、`dataplane`、`dataplane-brand`、`dataplane-gm`、`web` |
| `lua` | 改动了 `dataplane/lua/**`、`dataplane/build-syedge-lua.sh`、`internal/render/lua*`、`internal/layout/**`、`internal/instance/**`、`cmd/syedge-instance/**`、`scripts/ci/lua-*.sh`（已列出的三个）、`scripts/release/package.sh`、`.github/workflows/lua.yml` | 构建附加包并在 amd64 / arm64 × 标准版 / 国密版上跑验收 |
| `packages` | 改动了 `deploy/**`、`dataplane/**`、`scripts/release/**`、`cmd/syedge-agent/**`、`cmd/syedge-instance/**`、`cmd/syedge-wrapper/**`、`internal/wrapper/**`、`internal/instance/**`、`internal/layout/**`、`.github/workflows/packages.yml` | 构建 rpm / deb，在多个发行版容器中安装冒烟 |

`release`、`demo-e2e`、`scale`、`deps`、`upstream-check` 不由 PR 触发，本手册的任务不需要它们。
每个任务的“必需检查”一节写明该 PR 应当看到哪些工作流；应触发的工作流没有出现，说明改动的路径不对，先排查。

### 0.4 本地自检命令

```bash
make test
```

```bash
make brand
```

```bash
gofmt -l cmd internal dataplane
```

```bash
bash -n <改过的 shell 脚本>
```

### 0.5 PR 描述模板

```markdown
## 任务
T<编号> <任务名>（docs/dev/ops-experience-handbook.md）

## 改动
- <文件>：<一句话>

## 验证
- 本地：<执行过的命令与结果>
- CI：<应当通过的工作流与关键步骤>

## 与手册的差异
<没有就写“无”>
```

## 1. 背景与目标

运维同事拿 OpenResty 的使用习惯对照 SyEdge，反馈“安装和对接使用手感差很多”。经代码与文档核对，差距集中在：

| # | 问题 | 现状依据 | 对应任务 |
| --- | --- | --- | --- |
| 1 | 没有 5 分钟上手页，手册 1000 多行且源码编译排在安装之前 | `docs/install-maintenance.md` 第 3 节在第 4 节之前 | T2 |
| 2 | 文档示例全是 `/opt/syedge`，正式发布包默认前缀却是 `/home/atwasoft/syedge` | `.github/workflows/release.yml` 的 `prefix` 默认值 | T3 |
| 3 | 独立模式装完 Lua 附加包后仍不能写 `*_by_lua*`：基线配置没有加载模块，也没有文档说明 | `internal/render/templates/main.conf.tmpl` 只在 `.HasLua` 时生成加载指令 | T5、T7 |
| 4 | 没有从 OpenResty 迁移的对照说明（缺哪些模块 / 库、基线默认值更严格） | `dataplane/build-syedge.sh` 的 configure 参数、`main.conf.tmpl` | T4 |
| 5 | 安装结束后不提示安装位置、配置入口、日志位置和下一步命令 | `deploy/node/install.sh` 结尾 | T6 |
| 6 | 文档改动没有任何自动检查 | CI 中没有文档检查 | T1 |

不在本期范围内、需要产品决定的事项见第 4 节，**不要执行**。

## 2. 事实基线（写文档与代码时引用，动手前按“来源”核对）

### 2.1 数据面编译的模块

来源：`dataplane/build-syedge.sh` 中 `CONF=(…)` 的定义。

- 已启用：`http_ssl`、`http_v2`、`http_v3`、`http_realip`、`http_stub_status`、`http_gzip_static`、`http_sub`、
  `http_auth_request`、`stream`、`stream_ssl`、`stream_ssl_preread`、线程池、file aio；以及上游默认内置的 `gzip`、`proxy`、`rewrite`、`limit_req`、`limit_conn`、`map`、`geo` 等。
- 已关闭：`http_auth_basic`。
- 没有编译：`http_slice`、`http_mp4`、`http_dav`、`http_secure_link`、`http_addition`、`http_gunzip`、`http_random_index`、`stream_realip`、`mail`。
- 没有任何第三方 C 模块。OpenResty 自带而这里没有的常见指令：`more_set_headers` / `more_clear_headers`、`echo`、
  `set_escape_uri` 等 set-misc 指令、`redis2_*`、`memc_*`、`srcache_*`、`drizzle_*`、`rds_json`、`encrypted_session`。

### 2.2 Lua 附加包内容

来源：`dataplane/build-syedge-lua.sh`、`dataplane/lua/DEPS`、`dataplane/lua/lualib/`。

- 有：LuaJIT 2.1、`ndk_http_module.so`、`ngx_http_lua_module.so`（标准版与国密版各一套）、`resty.core`、`resty.lrucache`、
  `etos.init` / `etos.json` / `etos.jwt`。
- 没有：`cjson`（任何 C 模块都没有）、`resty.redis`、`resty.mysql`、`resty.http`、`resty.dns`、`resty.lock`、`resty.limit.*`、
  `resty.upstream.healthcheck`、`resty.websocket`、`resty.string`、stream 的 Lua 模块、`resty` 命令行、`opm`。
- 附加包 tarball 顶层只允许 `install.sh`、`uninstall.sh` 与 `addon/`；`addon/` 内的文件名由
  `internal/wrapper/lua.go` 的 `luaEntryRe` 限定。**不要往 tarball 里加新的顶层文件或 `addon/` 下的新类型文件。**
- `uninstall.sh` 用 `grep "modules/lua/"` 检查主配置来判断 Lua 是否仍在使用。

### 2.3 基线配置的默认值

来源：`internal/render/templates/main.conf.tmpl`。

| 项 | 基线值 | 上游默认值 |
| --- | --- | --- |
| 未匹配域名的请求 | HTTP 返回 444（断开），HTTPS 拒绝握手 | 落到第一个 server |
| `client_max_body_size` | `1m` | `1m`（相同，但 OpenResty 用户通常已调大） |
| `client_header_timeout` / `client_body_timeout` / `send_timeout` | `10s` | `60s` |
| `keepalive_timeout` | `15s` | `75s` |
| `proxy_connect_timeout` | `3s` | `60s` |
| `proxy_read_timeout` / `proxy_send_timeout` | `30s` | `60s` |
| `proxy_http_version` | `1.1` | `1.0` |
| `ssl_protocols` | `TLSv1.2 TLSv1.3` | 视版本而定 |
| `server_tokens` | `off` | `on` |
| 访问日志 | JSON（`edge_json`） | `combined` |
| http 层 `proxy_set_header` | `Host`、`X-Real-IP`、`X-Forwarded-For`、`X-Forwarded-Proto`、`X-Request-ID` | 无 |
| 限速区域 | `edge_per_ip`（20r/s）、`edge_conn` 已定义，站点内按需引用 | 无 |
| 状态页 | `127.0.0.1:<状态端口>`，默认 8081 | 无 |

location 内一旦写 `proxy_set_header`，http 层的 5 条全部失效，需要在该 location 重写。

### 2.4 路径

来源：`internal/layout/layout.go`、`deploy/node/install.sh`。

- 安装根目录记为 `R`，用 `sudo syedge-instance prefix` 查询（该命令在 `syedge-agent` 软件包中）。
- 本地构建未设置 `SYEDGE_PREFIX` 时 `R=/opt/syedge`；GitHub Actions 的发布包默认 `R=/home/atwasoft/syedge`。
- `R=/opt/syedge`（历史布局）时日志、缓存、agent 状态在 `/var/log/syedge`、`/var/cache/syedge`、`/var/lib/syedge-agent`；
  其他根目录下在 `R/logs`、`R/cache`、`R/agent`。
- 主配置 `R/conf/current/syedge.conf`，`conf/current` 是指向 `releases/<版本>` 的软链；初始版本为 `r000000`。

## 3. 任务

### T1 文档检查门禁

**目的**：让后面的文档任务可以被 Actions 验证。

**涉及文件**

- 新增 `scripts/ci/docs-check.sh`
- 新增 `scripts/ci/docs-required.txt`
- 修改 `.github/workflows/ci.yml`（新增 `docs` 任务）

**要求**

1. `scripts/ci/docs-check.sh` 用 bash 实现，只依赖 `grep`、`sed`、`awk`、`find`，不联网。检查两件事：
   - **相对链接有效**：扫描 `README.md` 与 `docs/**/*.md` 中形如 `](相对路径)` 与 `](相对路径#锚点)` 的 Markdown 链接，
     去掉 `#锚点` 后，目标文件或目录必须存在（相对该 Markdown 文件所在目录解析）。
     以 `http://`、`https://`、`mailto:`、`#` 开头的链接跳过。代码块（三个反引号之间）中的内容跳过。
   - **必需内容存在**：读取 `scripts/ci/docs-required.txt`，每行格式为 `<文件路径><TAB><固定字符串>`，
     `#` 开头与空行忽略；用 `grep -F` 确认文件中包含该字符串。
2. 任何一项不满足时输出 `FAIL <文件>: <原因>`，在 `GITHUB_ACTIONS` 非空时同时输出 `::error file=<文件>::<原因>`，最后以非零状态退出；
   全部通过时输出 `docs-check OK`。
3. `docs-required.txt` 初始内容只放现有文档里已经存在的两条，证明机制可用：
   - `docs/NOTICE<TAB>based on NGINX Open Source (BSD-2-Clause)`
   - `README.md<TAB>docs/install-maintenance.md`
4. `ci.yml` 新增任务，放在 `brand` 任务之后，写法与 `brand` 相同：

   ```yaml
     docs:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v5
         - run: ./scripts/ci/docs-check.sh
   ```

5. 现有文档若存在失效的相对链接，在本 PR 中修正链接（只改链接目标，不改正文），并在 PR 描述中列出。

**先写失败用例**：先提交脚本和一条故意指向不存在文件的 `docs-required.txt` 条目，确认本地运行失败；再改为正确内容。
（故意失败的那条不要推送到 PR，只在本地验证。）

**本地自检**：`bash -n scripts/ci/docs-check.sh`；`./scripts/ci/docs-check.sh` 输出 `docs-check OK`。

**必需检查**：`ci` 全部任务，其中新增的 `docs` 任务通过。

**验收**：把某个文档里的相对链接改坏后脚本失败并指出文件；恢复后通过。

---

### T2 快速上手页与手册瘦身

**目的**：给运维一页能照着敲完的上手说明。

**涉及文件**

- 新增 `docs/quickstart.md`
- 新增 `docs/build-from-source.md`
- 修改 `docs/install-maintenance.md`、`README.md`、`scripts/ci/docs-required.txt`

**要求**

1. `docs/quickstart.md` 不超过 120 行，包含且只包含下面四节，命令全部来自现有手册第 4、5、8 节，不得编造新命令或参数：
   - `## 1. 选包`：三行表格（控制台 / 边缘节点 / 可选的 Lua 附加包），指向 `release-assets.md`。
   - `## 2. 独立模式：5 条命令跑起来`：解压节点包 → `sudo ./install.sh` → 复制 `standalone-example.conf` → 校验 → reload。
     所有路径用 `R` 变量，开头给出 `R=$(sudo syedge-instance prefix)`。
   - `## 3. 接入控制台：3 步`：启动控制台（手册 5.1 的命令）→ 控制台“添加节点”生成命令 → 节点上执行。
   - `## 4. 常用命令对照`：左列为 OpenResty 用户熟悉的操作（校验配置、平滑重载、查看版本与编译参数、重开日志、查看错误日志），
     右列为 SyEdge 的命令。
2. 把 `docs/install-maintenance.md` 第 3 节（`## 3. 从源码编译` 到 `## 4. 安装边缘节点` 之前的全部内容）原样移到
   `docs/build-from-source.md`（标题层级整体上调一级，章节编号去掉 `3.` 前缀）。
   原位置保留 `## 3. 从源码编译` 标题和一句指向新文件的说明，**后面各节的编号不变**（其他文档与包内 README 按编号引用）。
3. `docs/install-maintenance.md` 开头（第一段之后）加一句指向 `quickstart.md` 的提示。
4. `README.md` 的文档列表中，在“安装维护手册”之前加一行“快速上手”。
5. 先在 `docs-required.txt` 增加（然后再写文档）：
   - `docs/quickstart.md<TAB>## 2. 独立模式`
   - `docs/quickstart.md<TAB>## 3. 接入控制台`
   - `docs/quickstart.md<TAB>syedge-instance prefix`
   - `docs/build-from-source.md<TAB>build-syedge.sh`
   - `README.md<TAB>docs/quickstart.md`
   - `docs/install-maintenance.md<TAB>build-from-source.md`

**必需检查**：`ci`（重点看 `docs`）。本任务不应触发 `lua`、`packages`。

**验收**：`grep -c '' docs/quickstart.md` 不超过 120；`docs/install-maintenance.md` 中 `## 4.` 到 `## 11.` 的标题文字与改动前逐字相同。

---

### T3 路径约定统一

**目的**：消除“文档写 `/opt/syedge`，实际装在别处”的困惑。

**涉及文件**：`docs/install-maintenance.md`、`docs/lua.md`、`docs/gm.md`、`scripts/ci/docs-required.txt`

**要求**

1. 在 `docs/install-maintenance.md` 第 1 节末尾新增小节 `### 路径约定`，内容按本手册 2.4 节写：`R` 的含义、如何查询、
   两种默认值各在什么情况下出现、历史布局与新布局下日志 / 缓存 / agent 状态目录的对应关系（用一张表）。
2. 第 8.1 节“关键目录和文件”的表格增加一列，给出非 `/opt/syedge` 根目录下的对应路径（`R/logs/error.log` 等）。
3. `docs/lua.md` 与 `docs/gm.md` 中出现 `/opt/syedge` 的示例，在首次出现处加一句“以下以 `/opt/syedge` 为例，实际根目录见安装维护手册的路径约定”，
   并链接过去。**不要批量替换示例里的路径**。
4. `docs-required.txt` 增加：
   - `docs/install-maintenance.md<TAB>### 路径约定`
   - `docs/install-maintenance.md<TAB>/home/atwasoft/syedge`

**必需检查**：`ci`（`docs`）。

---

### T4 从 OpenResty 迁移的对照说明

**目的**：让运维在迁移前就知道哪些能直接用、哪些要改、哪些做不了。

**涉及文件**

- 新增 `docs/migrate-from-openresty.md`
- 修改 `docs/install-maintenance.md`（8.5 节末尾加一行链接）、`docs/quickstart.md`（末尾加一行链接）、`scripts/ci/docs-required.txt`

**要求**

1. 内容只能来自本手册第 2 节的事实基线与现有文档（`docs/lua.md`、`docs/legacy-config-management.md`、手册 8.5 节）。
   写之前按 2.1–2.3 的“来源”核对一遍，代码与手册不一致时以代码为准。
2. 章节固定为：
   - `## 1. 能直接沿用的`：配置语法、命令行参数、信号、已启用模块（2.1）。
   - `## 2. 指令与模块差异`：一张表，列为“OpenResty 中的写法 / SyEdge 是否支持 / 替代做法”。至少覆盖 2.1 中列出的全部缺失模块与指令；
     替代做法只写确定可行的（例如 `more_set_headers` → server 级 `add_header` 或 Lua 的 `header_filter` 阶段；`auth_basic` → 外部鉴权规则），
     不确定的写“无，需评估”。
   - `## 3. Lua 差异`：2.2 的“有 / 没有”清单；独立模式与控制台托管模式的区别（托管模式的受限环境摘自 `docs/lua.md` 第 5 节，给链接不要全文复制）。
   - `## 4. 基线默认值差异`：2.3 的表格，并写明在独立模式下如何在自己的 `conf.d/*.conf` 的 server 内覆盖（例如上传接口调大 `client_max_body_size`、长请求调大 `proxy_read_timeout`）。
   - `## 5. 迁移步骤`：独立模式（逐个站点迁到 `conf.d`）与控制台托管（“配置导入”+ 观察接入）两条路线，各 5 步以内，链接到现有文档。
   - `## 6. 迁移前自查清单`：用 `grep` 在旧配置目录里查找不支持指令的命令示例（放在代码块里）。
3. `docs-required.txt` 增加本文件的六个二级标题，以及：
   - `docs/migrate-from-openresty.md<TAB>more_set_headers`
   - `docs/migrate-from-openresty.md<TAB>auth_basic`
   - `docs/migrate-from-openresty.md<TAB>resty.redis`
   - `docs/migrate-from-openresty.md<TAB>client_max_body_size`

**必需检查**：`ci`（`docs`）。

---

### T5 独立模式一条命令启用 Lua

**目的**：独立模式（未接入控制台）装完附加包后，能直接在 `conf.d/*.conf` 里写 `content_by_lua_block` 等原生指令，不需要手改主配置。

**涉及文件**

- 修改 `dataplane/lua/install.sh`、`dataplane/lua/uninstall.sh`
- 新增 `scripts/ci/lua-standalone.sh`
- 修改 `.github/workflows/lua.yml`
- 修改 `docs/lua.md`、`docs/install-maintenance.md`（7.1 节）、`scripts/ci/docs-required.txt`

**先读**：`dataplane/lua/install.sh` 全文、`dataplane/lua/uninstall.sh` 全文、`scripts/ci/lua-custom-prefix.sh`（学习它如何在 CI 机器上准备基础包、基线配置与实例）、
`.github/workflows/lua.yml` 的 `accept` 任务、`internal/layout/layout.go` 中 agent 状态目录的计算。

**设计（按此实现，不要另选方案）**

1. `install.sh` 新增两个互斥参数，在 `resolve_target` 的参数循环中解析，结果存入变量 `STANDALONE_ACTION`（空、`enable`、`disable`）：
   - `--standalone`：照常安装附加包，自检通过后执行“启用”；
   - `--standalone-disable`：不安装，只执行“停用”。
   `uninstall.sh` 在 `resolve_target` 之后发现 `STANDALONE_ACTION` 非空时报错退出（这两个参数只属于 `install.sh`）。
   不带新参数时，`install.sh` 与 `uninstall.sh` 的行为必须与改动前完全一致。
2. **启用**的前置检查，任一不满足即报错退出且不改动任何文件：
   - 目标实例的 `conf/current` 是软链，且指向的目录名为 `r000000`（已由控制台发布过的节点不允许）；
   - 目标实例的 agent 状态目录中不存在 `node.json`（已注册的节点不允许；状态目录按 `internal/layout/layout.go` 的规则计算，
     `install.sh` 中用同样的规则实现并写注释指明来源）；
   - 主配置 `conf/current/syedge.conf` 存在，且其中没有 `load_module`。
3. **启用**的动作：
   - 在主配置最前面插入两行（先 ndk 后 lua），路径用目标模块目录的绝对路径，必须包含子串 `modules/lua/`
     （`uninstall.sh` 靠它判断 Lua 仍在使用）：

     ```
     load_module  <模块目录>/lua/ndk_http_module.so;
     load_module  <模块目录>/lua/ngx_http_lua_module.so;
     ```

   - 新建 `conf/current/conf.d/00-lua-standalone.conf`（已存在则报错退出），内容为固定文本加路径：

     ```
     # 独立模式 Lua（install.sh --standalone 生成）。自己的 Lua 文件放在 <根目录>/lualib/ 下。
     lua_package_path   "<模块目录>/lua/lualib/?.lua;<根目录>/lualib/?.lua";
     lua_package_cpath  "<模块目录>/lua/lualib/?.disabled";
     ```

   - 创建 `<根目录>/lualib/` 目录（root 所有，0755），已存在则不动。
   - 写文件一律“写临时文件 → 设置与原文件相同的属主和权限（`chown --reference` / `chmod --reference`）→ `mv`”，不原地编辑。
   - 用真实的 `syedge -t -q` 校验目标实例的当前配置（命令形式与 `internal/render/standalone/STANDALONE.txt` 中的一致：带 `-p`、`-c`、`-e stderr`）。
     失败时恢复主配置、删除新建的 `00-lua-standalone.conf`，报错退出。
   - 成功后**不 reload**，只打印下一步命令（`sudo systemctl reload syedge`，具名实例为 `syedge@<实例>`）。
   - 路径只来自已通过 `resolve_target` 正则校验的 `ROOT` / `BASE_ROOT`，不接受任何新的用户输入。
4. **停用**的动作：删除主配置中上述两行 `load_module`（只删与启用时写入内容完全相同的行）、删除 `00-lua-standalone.conf`，然后 `syedge -t -q`；
   校验失败（说明站点配置仍在用 Lua 指令）时恢复原状并报错，提示先去掉站点里的 Lua 指令。不删除 `<根目录>/lualib/`。
5. `uninstall.sh` 的拒绝提示目前只说“先在控制台去掉 JWT 规则”。主配置含 `load_module` 且存在 `00-lua-standalone.conf` 时，
   改为提示先执行 `bash install.sh --standalone-disable`。
6. 脚本中的提示文字不得出现上游名（`dataplane/lua/install.sh` 在品牌检查范围内）。

**验收脚本 `scripts/ci/lua-standalone.sh`**（用法 `scripts/ci/lua-standalone.sh <dist-lua 目录> <standard|gmssl>`，环境准备方式照抄 `lua-custom-prefix.sh`）断言：

| # | 场景 | 期望 |
| --- | --- | --- |
| 1 | 不带新参数安装附加包后，在 `conf.d` 放一个含 `content_by_lua_block` 的站点，`syedge -t` | 失败（未知指令），证明问题存在 |
| 2 | `bash install.sh --standalone` | 成功；主配置前两行为 `load_module`；`00-lua-standalone.conf` 存在；主配置属主与权限和执行前相同 |
| 3 | 场景 1 的站点配置 `syedge -t`，reload 后 `curl` | 校验通过；响应体为 Lua 输出的固定字符串 |
| 4 | 在 `<根目录>/lualib/` 放一个 `hello.lua`，站点里 `require("hello")` | 请求返回该模块的输出 |
| 5 | `scripts/ci/standalone-lua-response-filter.conf` 复制到 `conf.d` 后 `syedge -t` | 通过 |
| 6 | 再执行一次 `--standalone` | 报错退出，文件不变 |
| 7 | `bash uninstall.sh` | 拒绝，提示 `--standalone-disable` |
| 8 | 站点仍在用 Lua 时 `--standalone-disable` | 报错退出，配置恢复原状，`syedge -t` 仍通过 |
| 9 | 去掉 Lua 站点后 `--standalone-disable`，再 `uninstall.sh` | 都成功；主配置与最初逐字节相同 |
| 10 | 把 `conf/current` 指到一个名为 `r000001` 的版本目录后 `--standalone` | 报错退出，不改动文件 |
| 11 | agent 状态目录放一个 `node.json` 后 `--standalone` | 报错退出，不改动文件 |
| 12 | 整个过程中 `syedge` master 的 PID | 不变（没有被重启或停止） |

`lua.yml` 的改动：`accept` 任务中在 “Lua shared and independent instance selection” 步骤之后加一步运行该脚本；
`push` 与 `pull_request` 两处 `paths` 列表各加一行 `"scripts/ci/lua-standalone.sh"`。

**文档**：`docs/lua.md` 的“独立模式手工 Lua”小节与手册 7.1 节补充 `--standalone` / `--standalone-disable` 的用法、前置条件与
“自己的 Lua 文件放在 `R/lualib/`”；`docs-required.txt` 增加 `docs/lua.md<TAB>--standalone-disable` 与 `docs/install-maintenance.md<TAB>install.sh --standalone`。

**先写失败用例**：先提交 `lua-standalone.sh` 与 `lua.yml` 的改动（此时 CI 的该步骤应失败在场景 2），再提交实现。

**本地自检**：`bash -n dataplane/lua/install.sh dataplane/lua/uninstall.sh scripts/ci/lua-standalone.sh`；`make brand`。

**必需检查**：`ci` 全部；`lua` 全部（4 组矩阵）；`packages` 全部（`dataplane/**` 会触发）。

**不在范围内**：不改 `internal/render/`、`internal/wrapper/`、基线模板；不新增 Lua 库；不放开托管模式的受限环境。

---

### T6 安装结束时打印摘要

**目的**：装完就知道东西在哪、下一步敲什么。

**涉及文件**：`deploy/node/install.sh`、`scripts/release/smoke-node.sh`（或 `packages.yml` 中调用节点安装的现有步骤所用脚本，先确认哪个脚本实际执行了 `install.sh`）

**要求**

1. `install.sh` 在全部步骤成功后（脚本末尾）打印固定格式的摘要，变量取自脚本已有的 `R`、`LOG_DIR`：

   ```
   == 安装完成
   安装根目录   <R>
   主配置       <R>/conf/current/syedge.conf
   站点配置目录 <R>/conf/current/conf.d/
   错误日志     <LOG_DIR>/error.log
   校验配置     sudo <R>/sbin/syedge -p <R> -c <R>/conf/current/syedge.conf -e stderr -t
   平滑重载     sudo systemctl reload syedge
   独立模式说明 <R>/share/doc/STANDALONE.txt
   ```

2. 不改变安装逻辑、退出码和已有输出的顺序；摘要打印在已有的“下一步：在控制台…”提示之后。
3. 先在冒烟脚本里加断言（安装输出包含 `== 安装完成` 与 `校验配置`，且摘要里的校验命令实际执行能通过），确认失败后再实现。

**必需检查**：`ci` 全部；`packages` 全部。

**不在范围内**：rpm / deb 的 postinstall 输出（包管理器的输出习惯不同，另议）。

---

### T7 随包说明同步（依赖 T5、T6 已合并）

**目的**：装在机器上的说明与仓库文档一致。

**涉及文件**：`internal/render/standalone/STANDALONE.txt`、`internal/render/standalone/example.conf`、`internal/render/standalone_test.go`、`scripts/release/README-node.txt`

**要求**

1. `STANDALONE.txt` 新增两小节（占位符沿用 `@ID@`、`@PREFIX@`，不得写死产品名或路径）：
   - “默认值与常见调整”：摘 2.3 表中最常被问到的四项（未匹配域名断开、请求体 1m、代理读超时 30s、location 内 `proxy_set_header` 需重写 5 条）及覆盖写法；
   - “Lua”：安装附加包并启用的两条命令（`bash install.sh --standalone`、reload），以及“自己的 Lua 文件放在 `@PREFIX@/lualib/`”。
2. `STANDALONE.txt` 中“使用规划中的‘导入现有配置’功能”改为指向已有的控制台“配置导入”。
3. `example.conf` 末尾增加一个注释掉的 Lua 示例 location（`content_by_lua_block` 输出一行文本），注明需先启用 Lua。
4. `README-node.txt` 的“独立模式”一段补一行 Lua 启用命令。
5. 这些文件在品牌检查范围内：不得出现上游名（文件末尾已有的标准归因句除外）。
6. 先在 `standalone_test.go` 中加断言（`StandaloneFiles()` 返回的说明包含 `--standalone`、`lualib`，且不含未替换的 `@ID@` / `@PREFIX@`），确认失败后再改文本。

**本地自检**：`go test -mod=vendor ./internal/render/ -run 'Standalone|PackageDefaults'`；`make brand`。

**必需检查**：`ci` 全部（`go`、`brand`、`dataplane` 中的渲染器真实校验）；`packages` 全部（`scripts/release/**` 会触发）。

## 4. 需要产品决定的事项（不要执行）

下面每一项都涉及安全边界、授权或发布流程，先由负责人决定，再另写任务。

| # | 事项 | 需要决定什么 |
| --- | --- | --- |
| D1 | Lua 库扩展包（`cjson`、`resty.http`、`resty.redis` 等） | 是否只在独立模式提供；是否允许 C 模块（现在 `lua_package_cpath` 被有意禁用）；每个库的许可与审核流程；是否需要授权功能 |
| D2 | `syedge-lua` 的 rpm / deb | postinstall 是否允许自动执行模块切换与 `syedge -t`；与数据面版本的依赖如何在包元数据中表达 |
| D3 | 国密版首次安装节点包 | 目前国密版只能经控制台用厂商签名的升级包切换；是否提供可直接安装的国密节点包，签名与授权如何处理 |
| D4 | 发布包默认前缀 | 是否把 `release.yml` 的默认值从 `/home/atwasoft/syedge` 改回 `/opt/syedge`，客户专用前缀改为手动触发时传入 |
| D5 | 环境变量与库名统一 | 控制台用 `ETOS_*`、agent 用 `SYEDGE_*`、Lua 库名 `etos`；是否统一，旧名兼容多久 |
| D6 | 软件源 | 是否提供离线 yum / apt 源目录或一条命令的下载安装脚本，由谁托管 |
| D7 | 第三方 C 模块（headers-more 等） | 是否作为动态模块随附加包提供；与“数据面只做改名构建”的原则如何协调 |

### 4.1 建议（未决定）

以下是执行完 T1–T7 后给出的建议，不是决定，仍需负责人确认后另写任务。

- D1：建议作为下一轮的首选。只在独立模式与原生兼容配置包（legacy）中提供，先放纯 Lua 库（如 resty.http、resty.redis，许可逐个核对），C 模块暂不开放；控制台托管的受限环境不变。理由：迁移指南里这是现网脚本搬不过来的最大缺口。（进展：第一阶段已实现，随附 10 个纯 Lua 库，见 `docs/lua.md` 的“随附的 Lua 库”一节；C 模块与第二阶段库仍未决定。）
- D2：建议暂缓。tar 包加 install.sh（含 --standalone）已覆盖安装与启用，包管理器的 postinstall 自动切换模块风险较高。
- D3：建议暂缓，等签名与授权方案确定后再做。
- D4：建议维持现状。已有客户按现在的默认前缀部署，文档已用安装根目录 R 的约定消除歧义。
- D5：建议不改。改名涉及兼容期，收益小；文档已说明各自的前缀。
- D6：建议下一轮评估，取决于由谁托管。
- D7：建议不做。与“数据面只做改名构建”的原则冲突；迁移指南已给出替代写法。

## 5. 完成定义

一个任务完成，当且仅当：

1. PR 只包含该任务的改动，描述按 0.5 的模板填写；
2. 0.3 表中该任务“必需检查”列出的工作流全部出现且通过；
3. 任务“验收”与验收脚本中的每一条断言都有对应的自动检查（不是人工声明）；
4. 没有触碰 0.2 的任何一条。

全部任务合并后，请一位没有参与开发的运维同事只看 `docs/quickstart.md` 完成一次独立模式安装、启用 Lua、写一个 Lua 接口，
记录卡住的地方，作为下一轮改进的输入。

## 6. 执行结果

| 任务 | PR | 状态 |
| --- | --- | --- |
| T1 文档检查门禁 | [#62](https://github.com/xiaoguan521/edgetraffic-os/pull/62) | 已合并 |
| T2 快速上手页与手册瘦身 | [#63](https://github.com/xiaoguan521/edgetraffic-os/pull/63) | 已合并 |
| T3 路径约定统一 | [#65](https://github.com/xiaoguan521/edgetraffic-os/pull/65) | 已合并 |
| T4 从 OpenResty 迁移的对照说明 | [#66](https://github.com/xiaoguan521/edgetraffic-os/pull/66) | 已合并 |
| T5 独立模式一条命令启用 Lua | [#67](https://github.com/xiaoguan521/edgetraffic-os/pull/67) | 已合并 |
| T6 安装结束时打印摘要 | [#68](https://github.com/xiaoguan521/edgetraffic-os/pull/68) | 已合并 |
| T7 随包说明同步 | [#69](https://github.com/xiaoguan521/edgetraffic-os/pull/69) | 已合并 |
| 原生兼容配置包的边界收紧（非手册任务） | [#64](https://github.com/xiaoguan521/edgetraffic-os/pull/64) | 已合并 |

第 5 节末尾的运维同事实测尚未进行。
