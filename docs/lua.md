# 访问规则与 Lua 扩展（P10）

规格书 12.5。分两层：

| 能力 | 依赖 | 授权 |
| --- | --- | --- |
| 原生规则积木：IP / IP 集（地域）/ 请求头 / User-Agent / 路径 / 方法 → 拒绝 / 跳转 / 限速 / 外部鉴权 | 只用官方模块，标准版与国密版基础包都能用 | `core`（所有版本） |
| Lua 规则：JWT 校验（HS256 / RS256） | 节点安装 `syedge-lua` 附加包 | `lua`（专业版、企业版） |
| 自定义 Lua 脚本：路由级，rewrite / access / content / header_filter / body_filter 阶段（第 5 节） | 节点安装 `syedge-lua` 附加包 | `lua`（专业版、企业版） |

配置中的 Lua 代码只由 `internal/render` 的固定模板生成：
- 参数写成 `\ddd` 转义的字面量；
- 密钥只写进版本目录的 `lua/*.key` 文件（0640，预览中不显示）；
- 自定义脚本的文本不进入配置，只写成版本目录中的 `lua/script-*.lua` 文件，由固定模板加载（第 5 节）。

只有用到 Lua（站点有 JWT 规则或路由有生效的自定义脚本）的版本才加载 `ndk_http_module` 与 `ngx_http_lua_module`（先 ndk 后 lua）
并生成 `lua_package_path` 与 `init_by_lua_block`；没有用到 Lua 的版本不加载模块，这是按需加载，不是探测失败。

### 独立模式手工 Lua（仅数据面）

独立模式不接入控制台时，管理员可以在手工维护的 `conf.d/*.conf` 中直接使用已安装 Lua 模块的原生阶段指令，例如 `access_by_lua_block`、`rewrite_by_lua_block`、`header_filter_by_lua_block` 和 `body_filter_by_lua_block`。这属于受信任运维人员维护的数据面配置，不会被控制台渲染、预览或纳入控制台脚本编辑入口；节点注册并由控制台接管后，手工配置会按独立模式接管规则处理。

独立模式验收夹具：`scripts/ci/standalone-lua-response-filter.conf`。它验证请求阶段的放行/拒绝、响应头新增与 `Content-Length` 清除，以及响应体改写；实际运行仍须在已安装匹配 `syedge-lua` 的独立节点上先执行 `syedge -t`。

#### 一条命令启用（推荐）

附加包解包后，在其目录内以 root 执行（不需要手改主配置）：

```bash
sudo bash install.sh --standalone          # 安装附加包，并启用独立模式 Lua
sudo systemctl reload syedge               # 启用只做 syedge -t 校验，不会自动 reload
sudo bash install.sh --standalone-disable  # 停用（只停用，不卸载附加包）
```

前置条件（任一不满足即报错退出，且不改动任何文件）：

- 节点未注册到控制台：agent 状态目录中没有 `node.json`；
- `conf/current` 指向初始版本 `r000000`，即控制台未发布过配置；
- 主配置 `conf/current/syedge.conf` 中没有 `load_module`，即没有手工加载过模块。

启用会做的事：

- 在主配置最前面插入两行 `load_module`，分别加载 `R/modules/lua/ndk_http_module.so` 与 `R/modules/lua/ngx_http_lua_module.so`（先 ndk 后 lua）；
- 新建 `conf.d/00-lua-standalone.conf`，设置 `lua_package_path` 与 `lua_package_cpath`；
- 创建 `R/lualib/`（已存在则不动），用于放自己的 Lua 文件；
- 用真实的 `syedge -t` 校验，失败则恢复主配置并删除新建的文件。

自己的 Lua 文件放在 `R/lualib/` 下，例如 `R/lualib/hello.lua`，站点里 `require("hello")`。具名实例用 `--instance <实例名>`，其 `R` 为 `R/instances/<实例名>`，reload 时使用 `syedge@<实例名>`。`R` 是安装根目录，见[安装维护手册的路径约定](install-maintenance.md#路径约定)。

停用前先删除站点里的 Lua 指令（如 `access_by_lua_block`），否则停用的校验会失败并恢复原状。停用会删除上述两行 `load_module` 与 `conf.d/00-lua-standalone.conf`，但保留 `R/lualib/`。独立模式 Lua 仍启用时，`uninstall.sh` 会拒绝卸载并提示先执行 `--standalone-disable`。

已有独立配置需要先接入控制台观察时，使用节点组的 `observe` / `legacy` 接管方式；它们只做导入分析、审计和漂移观察，不会把原生 Lua 配置自动改写成受限脚本，也不会自动覆盖节点。操作步骤见 [现有配置的观察接入与 Lua 兼容](legacy-config-management.md)。

## 原生规则积木

每条规则由 1–8 个条件（同时满足）和 1 个动作组成，每个站点最多 32 条。

执行顺序由请求处理阶段决定：
1. 拒绝 / 跳转（rewrite）；
2. JWT（rewrite，紧接在拒绝 / 跳转之后；用 set_by_lua 同步校验，只有命中规则的请求才进入 Lua）；
3. 限速（preaccess）；
4. 外部鉴权（access）。

同一阶段内按规则顺序执行。ACME 验证路径 `/.well-known/acme-challenge/` 不受任何规则影响。

- **条件**：
  - IP / 网段（IPv4 / IPv6）；
  - IP 集；
  - 请求头（等于 / 开头是 / 包含 / 正则 / 存在）；
  - User-Agent（不区分大小写）；
  - 路径（前缀 / 精确 / 正则）；
  - 请求方法。

  每个条件都可以取反。真实客户端地址按节点组的“CDN / LB 回源网段”（real_ip）计算。
- **动作**：
  - 拒绝：401 / 403 / 404 / 429 / 444 / 451 / 503；
  - 跳转：301 / 302 / 307 / 308，可保留原路径；
  - 限速：按 IP 或按某个请求头计数，每秒或每分钟，可设突发；
  - 外部鉴权：对节点组的某个上游服务发子请求，不带请求体，携带 `X-Original-URI` / `X-Original-Method`。2xx 放行，401 / 403 原样返回。规则未命中时不访问鉴权服务。
- **IP 集与地域库**（菜单“IP 集与地域库”）：
  - 手工维护网段；
  - 或从 RIR 公开的分配统计文件（APNIC 等的 `delegated-*-extended-latest`）按国家 / 地区代码导入，同名 IP 集整体替换，定期重新导入即可更新；
  - 只有被规则引用的 IP 集才会渲染到节点。

## syedge-lua 附加包

一个包同时适配标准版与国密版基础包，内容固定为：
- LuaJIT 2.1（OpenResty 分支）；
- ngx_devel_kit、OpenResty 的 lua 模块（动态模块）；
- lua-resty-core、lua-resty-lrucache；
- 审核过的规则库 `etos`（JSON 解析、JWT 校验、初始化与沙箱）。

版本与提交哈希锁定在 `dataplane/lua/DEPS`。syedge 核心与改名 patch 不变。

基础包为此做了两处调整，都不改变不装附加包时的行为：
- 编译外部鉴权需要的 `auth_request` 模块；
- 固定导出附加包需要的 OpenSSL / 铜锁函数（`dataplane/lua/base-exports.txt`）。

### 安装与卸载（节点上以 root 执行）

```
mkdir syedge-lua && tar -C syedge-lua -xzf syedge-lua-1.30.6-x86_64.tar.gz && cd syedge-lua
sudo bash install.sh      # 多个实例时交互选择；自动选择标准版 / 国密版模块并用 syedge -t 自检
sudo bash uninstall.sh    # 当前配置仍使用 Lua 时拒绝；先在控制台去掉规则并发布
# 非交互环境需指定目标；默认实例名为 default：
sudo bash install.sh --instance blue
sudo bash uninstall.sh --instance blue
```

以下以 `/opt/syedge` 为例，实际根目录见 [安装维护手册的路径约定](install-maintenance.md#路径约定)。

安装的具体行为：
- system 模式自动识别节点安装根目录 `R`（默认为 `/opt/syedge`，也可以是 `/home/atwasoft/node`），无需再次传入路径；只有一个实例时自动选择，多个实例时交互选择；非交互环境必须用 `--instance` 指定；
- 共享实例的模块位于 `R/modules/lua`，安装或卸载会影响同一根目录的所有共享实例；独立实例 `blue` 的模块位于 `R/instances/blue/modules/lua`，只影响该实例。卸载前检查使用该模块的实例配置；
- 免 root 的 user 模式需指定根目录，例如 `SYEDGE_ROOT=/home/alice/node bash install.sh --instance blue`（卸载时同样设置 `SYEDGE_ROOT`）；
- 安装、卸载都不重启、不停止 syedge；
- agent 在心跳中上报附加包，节点列表显示“Lua”标签。

### 发布门禁与回退

满足下面两点，控制台才允许发布含 JWT 规则或自定义 Lua 脚本的版本：
1. 授权包含 `lua`；
2. 节点组内每个节点都上报了与其基础变体匹配的附加包。

否则预览与发布报 `LUA_ADDON_REQUIRED`，并列出缺少附加包的节点。

万一含 Lua 的版本到达未装附加包的节点，`syedge -t` 会因找不到模块而失败，agent 自动回退到原版本。

### 数据面热升级

模块与 syedge 二进制版本强绑定，所以装有附加包的节点升级时要带上同版本的附加包：
1. 厂商离线签名两个包：

   ```
   syedge-license-signer sign-package ... -name syedge     -version <版本> -arch amd64 -variant standard|gmssl
   syedge-license-signer sign-package ... -name syedge-lua -version <版本> -arch amd64 -variant lua
   ```

   附加包的 `addon/VERSION` 中的版本必须与 `<版本>` 一致（`build-syedge-lua.sh -v <版本>`）。
2. 两个包都导入“数据面版本库”。缺少附加包时，控制台拒绝开始升级。
3. 升级过程：
   - wrapper 以 root 验签后，先把模块软链切到目标版本，再用新二进制 `-t`，然后发 USR2；
   - 中止时，先把软链切回，再让旧 master 重新加载配置；
   - 任何一步失败都恢复原状。

未装附加包的节点升级流程不变。

## JWT 规则

- **令牌来源**：`Authorization: Bearer <令牌>` 或指定的 Cookie。
- **只接受配置的算法**：`alg=none` 与算法混淆（拿 RS256 公钥当 HS256 密钥）都会被拒绝。
- **校验项**：
  - 必须有 `exp`；
  - 校验 `nbf`，以及配置了的 `iss` / `aud`；
  - 可设置 0–300 秒的时钟偏差。
- **密钥要求**：
  - HS256 密钥至少 32 个字符，控制台加密保存，只写不读，响应中只显示“已设置”；
  - RS256 公钥至少 2048 位。
- **转发身份**：校验通过后，可以把 `sub` 写入指定请求头转发给上游。客户端自带的同名头总会先被清除。
- **结果**：失败返回 401，带 `WWW-Authenticate: Bearer error="invalid_token"`。
- **缓存**：校验结果按 worker 缓存，最多 60 秒，且不超过令牌有效期。
- **沙箱**：初始化读取密钥后，关闭文件、进程、代码加载、C 模块与 FFI 的访问。验收中有专门的用例检查。

## 第 5 节 自定义 Lua 脚本（2026-11-07 决定受控开放）

可视化规则覆盖不到的需求（例如由 Lua 直接生成响应、按复杂条件放行、改写响应头或响应体），可以给某个站点的某条路由加一段 Lua 脚本。
入口：“自定义片段 → 新建片段 → 插入点：Lua 脚本（路由）”，选择站点、路由与执行阶段。

**权限与审批**
- 只有**管理员**可以提交、修改、删除、审批 Lua 脚本；必须由**另一位管理员**批准（提交人不能批准自己的变更），批准后随下一次发布生效；
- 每次提交、批准、驳回、删除都写审计（含前后差异与 sha256）；
- 需要授权功能 `lua`；组内每个节点都必须装有与基础变体匹配的附加包，否则预览与发布报 `LUA_ADDON_REQUIRED`；
- 授权不含 `lua` 时仍可以申请删除已有脚本。

**执行阶段**：每条路由每个阶段一段脚本，最大 16 KiB，只接受 UTF-8 文本（不接受字节码）。

| 阶段 | 用途 | 说明 |
| --- | --- | --- |
| `rewrite` | 改写请求（`ngx.req.set_uri` 等） | |
| `access` | 访问控制（`ngx.exit(403)`） | |
| `content` | 生成响应（`ngx.say`） | 接管该路由的响应，路由原来的静态 / 代理不再生效 |
| `header_filter` | 修改响应头 | 跳转 / 拒绝路由只能用这两个阶段（它们在 rewrite 阶段就已返回） |
| `body_filter` | 修改响应体（`ngx.arg[1]`） | |

**渲染方式**：脚本写成版本目录中的 `lua/script-<站点>-<路由目录>-<阶段>.lua`，location 中只有渲染器生成的固定调用
`<阶段>_by_lua_block { return require("etos.script").run("<编号>") }`；脚本文本无法注入配置。

**运行环境**（`init_by_lua_block` 中的固定代码，在 `etos.init` 收紧环境之前编译脚本）：
- 可用：`ngx`（`ngx.socket`、`ngx.run_worker_thread` 除外：脚本不能自行建立网络连接，绕过节点组的回源网段限制）、`string`（无 `dump`）、
  `table`、`math`、`bit`、`coroutine`、`os.time` / `os.date` / `os.clock` / `os.difftime`、`require("etos.json")`，以及
  `pairs`、`pcall`、`tostring` 等基础函数；
- 不可用：文件（`io`）、命令（`os.execute`）、加载代码（`load` / `loadstring` / `dofile`）、其他模块（`require` 只允许 `etos.json`）、
  FFI、`debug`、`package`、`jit`、`getmetatable` / `rawset` / `setfenv`；
- 不能写全局变量（请用 `local`），请求之间不共享状态；标准库、`ngx` 的顶层字段与子表（`ngx.req`、`ngx.re` …）只读（`ngx.status`、`ngx.ctx` 可写；`ngx.var`、`ngx.header`、`ngx.arg` 按请求读写）。

**出错时**
- 语法错误：`syedge -t` 不执行 init 阶段，错误在 reload 时出现。master 不加载新配置、继续以当前配置服务；agent 从 error.log
  识别 `init_by_lua error: ... lua script syntax error: <编号>:<行>: ...` 后立即回滚，发布记录显示为“已回滚”并带这行错误；
- 运行时错误：记入该实例的 error.log（含脚本编号与行号），请求返回 500；其他路由不受影响。

**安全边界**：受限环境挡住了文件、进程、代码加载与网络访问，但管理员仍可能写出死循环、大量占用内存或泄露请求数据的脚本。
因此脚本只允许可信的管理员编写，并由另一位管理员审阅后批准。

**示例**（路由 `/lua`，阶段 content）：

```lua
ngx.header.content_type = "text/html"
ngx.say("<h1>Hello from Lua</h1><p>这条响应由 Lua 脚本生成。</p>")
```

## 验收（CI：`.github/workflows/lua.yml`）

amd64 / arm64 × 标准版 / 国密版共 4 组，在干净的机器上以生产路径运行 `scripts/ci/lua-acceptance.sh`，覆盖：
- 模块加载与真实 `syedge -t`（含渲染器黄金用例）；
- 原生规则、外部鉴权、限速；
- JWT HS256 / RS256，以及各种非法令牌；
- 自定义 Lua 脚本：content / access / header_filter 阶段、受限环境、运行时错误返回 500 并记录、语法错误在 reload 时被拒绝且当前配置继续服务；
- 沙箱；
- reload、带附加包的热升级与中止；
- 标准 HTTPS 与国密 NTLS；
- 安装、拒绝卸载、卸载；
- 品牌检查：模块目录 `/opt/syedge/modules/lua-*/` 之外，二进制、配置、日志中没有上游名；
- 性能：开启三条常用规则（IP 拒绝、UA 拒绝、JWT 校验）后 QPS 下降不超过 15%。压测口径（代表性负载）：
  - 后端返回 4 KB 响应，前端 syedge、后端与 wrk 在整机上运行，不绑核（与生产部署一致）；
  - 按 ABBA 顺序（无规则、有规则、有规则、无规则）跑 4 组，每次 5 秒；
  - 取 8 个相邻配对比值的中位数，消除机器前后漂移的影响；
  - 压测使用同一个有效令牌，测的是缓存命中路径（即请求阶段与 Lua/syedge 边界开销）。验签本身由功能用例覆盖。

  另外报告一组隔离最坏情况作为参考（不作门槛）：前端只绑 2 个 CPU 且是唯一瓶颈。此时每个请求进入 Lua 的固定开销
  （约 5–7 µs）占比被放大，arm64 上的下降约 20%。请求处理越重（TLS、更大的响应、真实后端延迟），这部分占比越小。

  CI 中 QPS 门槛只在 x86_64 上拦截。GitHub 的 arm64 共享机器吞吐抖动过大（同一配置的几次结果可相差数十倍），
  在那里只报告结果（`PERF_GATE=0`，输出 warning）。arm64 的性能验收在专用 ARM 实机上执行：

  ```bash
  scripts/ci/lua-acceptance.sh dist-lua standard   # 默认 PERF_GATE=1，超过 15% 即失败
  ```
