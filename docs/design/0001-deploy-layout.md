# 设计 0001：部署布局 —— 同机多实例、非 root 运行、品牌可定制

- 状态：**已实现（P11-A ~ P11-D，PR #30–#32；D5 运行时不使用 root、D7 自定义品牌 Server 头已按决策实现）**；评审时 D1、D2、D6 按建议执行，其余待决策项按建议默认；3.2 节在实现时修订（评审通过后拆为 P11-A ~ P11-D 开发，并按第 9 节修改 `CLAUDE.md`）
- 日期：2026-11-05
- 涉及规格书章节：4 数据面、5 agent、6 渲染器、7 控制台、8 授权、9 安全、12 路线
- 关联：ADR 0001（数据面只改名）、ADR 0002（agent 只主动外连）、提交 9b0716a（多实例 wrapper 初版）

## 1. 需求与目标

| 编号 | 需求 | 生产发布标准 |
| --- | --- | --- |
| R1 | 一台机器部署多个数据面实例，各用不同端口 | 每个实例可被控制台独立发布、reload、热升级、监控和卸载，任何操作不影响同机其他实例 |
| R2 | 没有 root 权限也能安装和使用 | 普通用户从安装、注册、发布、热升级到卸载全程不需要 root 或 sudo，运行中的进程没有一个属于 root |
| R3 | 数据面的程序名和进程名可以定制，例如 SyEdge | 命令、进程标题、路径、服务单元、安装包、界面、日志、错误页和 Server 头中都只出现定制名，不残留 syedge |

三个需求改的是同一件事：**程序叫什么、文件放在哪、以谁的身份运行、谁能给谁发信号**。现在这些约定散落在 42 个 Go 文件（203 处）、模板、打包脚本和 sudoers 里，而且都是写死的。本设计把它们收敛为一个统一的模型，称为部署布局（Layout），三个需求都在这个模型上实现。

不在本文范围：控制台自身的多实例部署。控制台本来就以普通用户身份运行（端口 8443/9443，`-data` 可配置），R2 对控制台只需要补一个免安装包，见 6.6 节。

## 2. 现状与差距

| 位置 | 现状 | 对 R1 | 对 R2 | 对 R3 |
| --- | --- | --- | --- | --- |
| `syedge.conf.tmpl` | `pid`、`error_log`、`access_log` 是绝对路径；`listen 80/443`；状态页固定为 `127.0.0.1:8081`；有 `user syedge` | 实例之间冲突 | 普通用户无法监听 80/443，也写不了 `/run`、`/var/log` | 路径里带名字 |
| `site.conf.tmpl` | `listen 80/443`；`alias /opt/syedge/conf/current/...` | 端口冲突，路径错到默认实例 | 同上 | 同上 |
| 国密与 Lua 模板 | 证书、模块用 `/opt/syedge/...` 绝对路径 | 指向错误的实例 | 目录不存在 | 同上 |
| `build-syedge.sh` | 编译期默认路径都在 `/opt/syedge`、`/run/syedge`、`/var/log/syedge`、`/var/cache/syedge` | 需要在运行时覆盖 | 同左 | 名字编译在二进制里 |
| 改名 patch | 64 处字符串常量写死为 `syedge`，其中部分带长度常量，HPACK 编码字节是预先算好的 | — | — | 换名要重新生成 patch |
| agent `main.go` | PID、日志、状态地址、wrapper 写死；只有 `-conf-root` 可改 | 无法托管第二个实例 | 依赖 sudo | 名字写死 |
| wrapper 与 sudoers | 路径写死；9b0716a 加了 `--instance`，同时新增一条 `--instance *` 通配 sudoers | 任一 agent 都能操作任一实例 | 依赖 sudo | 名字写死 |
| 升级状态 | 二进制共用（`sbin/syedge` 软链到 `versions/`）；`state.json` 放在共用的 `versions/` 下 | 多个实例互相覆盖升级状态 | — | — |
| 打包 | 只有一套 systemd 单元、logrotate、tmpfiles、nftables 规则，都只针对 80/443 | 无法新增或删除实例 | 必须 root 安装 | 包名写死 |
| 控制台模型 | 一个节点就是一台机器；同组节点拿到同一个配置包 | 没有主机和实例的概念 | — | 界面文案写死 |

**提交 9b0716a 的具体问题（应先处理，见 10.1 节）：**

1. `syedge-agent@.service` 传了 agent 并不认识的 `-data`、`-pid-file`、`-lock` 参数，agent 一启动就会退出。
2. wrapper 对实例执行 `syedge -c <实例配置> -s reload`。syedge 按配置里的 `pid` 找进程，而渲染出的 `pid` 是 `/run/syedge/syedge.pid`，**结果信号发给了默认实例**。
3. sudoers 中 `--instance *` 这一行的通配覆盖了全部子命令，任一实例的 agent 都能操作所有实例。这违反 `CLAUDE.md`“只有 test 与 upgrade-start 带通配”的契约。
4. 升级状态 `state.json` 由各实例共用，并发或先后升级时会互相覆盖，中止升级可能切回错误的版本。

这几处也是当时“多实例安装、升级、卸载仍未达到生产发布要求”这一结论的具体原因。

## 3. 核心设计：部署布局

### 3.1 四个维度

一个数据面实例的全部路径、用户和单元名，由下面四个维度唯一决定：

| 维度 | 取值 | 决定于 |
| --- | --- | --- |
| 品牌 `B` | 例如 `syedge`（默认）、`syedge` | **构建时**写入所有程序，见第 4 节 |
| 模式 `M` | `system`（root 安装，当前方式）或 `user`（普通用户安装） | 安装时选择 |
| 根目录 `R` | system 模式默认 `/opt/B`，也可在首次安装前由 `/etc/B/prefix` 指定；user 模式由用户指定，例如 `~/syedge` | 安装时确定 |
| 实例名 `I` | 空字符串表示默认实例；具名实例匹配 `^[a-z][a-z0-9-]{0,15}$`，`default` 为保留名 | `B-instance add` 时确定 |

代码中只有 `internal/layout` 一个包负责根据 `(B, M, R, I)` 计算路径。agent、wrapper、实例管理命令和渲染器都调用它，**任何地方都不允许再拼写路径或写死程序名**。wrapper 永远只接收实例名，不接收路径，路径由它自己算出，这是保持提权安全的关键。

### 3.2 按档案渲染绝对路径（实现时修订）

> 初稿方案是“配置一律用相对路径，以 `-p` 前缀启动”。实现时发现它要求节点上先有 `run/`、`logs/`、`cache/` 软链：控制台先升级、节点安装包后升级时，新配置的 `-t` 会失败，发布被卡住。因此改为下面的方案（P11-A 第二步，PR #26）。

- 渲染器的输入带上节点组的**实例档案**（3.4 节）。pid、错误日志、访问日志、四层代理日志、临时目录、静态资源 `alias`、国密证书路径、Lua 模块与密钥路径，全部由 `internal/layout` 根据档案算出绝对路径。
- 同组节点共用同一个档案（5.1 节原则 1），所以同组节点拿到的配置包仍然逐字节相同。
- 档案为零值（默认实例）时，渲染结果与引入档案前**逐字节相同**，已部署节点不需要任何迁移。
- 临时目录与编译期默认值不同时，才显式写出 `*_temp_path`；只有 system 模式才输出 `user` 指令。
- `load_module` 在 system 模式下仍相对于编译期前缀（同机实例共用 `/opt/B/modules`），在 user 模式下用绝对路径。
- 启动方式：默认实例不变；具名实例和 user 模式以 `-c <实例的 current 配置>` 启动，user 模式另加 `-e <实例错误日志>`，避免启动早期写编译期的 `/var/log/B`。

### 3.3 路径表

**system 模式**（root 安装；`B=syedge`、默认实例时，路径与现在完全一致）

| 内容 | 默认实例 | 具名实例 `I` |
| --- | --- | --- |
| 实例前缀 `P` | `/opt/B` | `/opt/B/instances/I` |
| 配置版本 | `P/conf/releases/`、`P/conf/current` | 同左 |
| 运行目录（pid） | `/run/B` | `/run/B-I` |
| 日志目录 | `/var/log/B` | `/var/log/B-I` |
| 临时目录 | `/var/cache/B` | `/var/cache/B-I` |
| agent 状态目录 | `/var/lib/B-agent` | `/var/lib/B-agent-I` |
| 数据面服务单元 | `B.service` | `B@I.service` |
| agent 服务单元 | `B-agent.service` | `B-agent@I.service` |
| agent 运行用户 | `B-agent` | `B-agent-I`（每个实例单独一个用户，见 5.5 节） |
| worker 运行用户 | `B` | `B`（共用） |
| 本机共用部分 | 二进制 `/opt/B/sbin/B`（软链到 `versions/`）、版本库 `/opt/B/versions/`、Lua 模块 `/opt/B/modules/`、`/usr/libexec/B/B-wrapper`、`/usr/bin/B-agent`、`/usr/sbin/B-instance` | |

`/run` 下的目录由 systemd 的 `RuntimeDirectory=` 或 tmpfiles 在开机时创建。

#### system 模式的自定义安装根目录

root 安装仍默认使用 `/opt/B`。如果发行版的 `/opt` 不适合放置运行数据，可在首次安装前由 root 写入 `/etc/B/prefix`，内容为一行规范化绝对路径 `R`（不能是 `/`）。标记文件必须是 root 所有、0644，`R` 及其已存在的父目录必须是 root 所有且不可被组或其他用户写入；读取方校验失败时拒绝运行。没有标记文件时按默认根目录处理。

`R` 必须是专用目录，不能选 `/usr`、`/opt`、`/var`、`/home` 等系统共享目录本身，或系统程序、配置目录及其子目录；`/root` 不用于 system 模式。`R` 位于 `/home/<子目录>` 时，支持绑定映射的 systemd 只暴露该目录；较旧 systemd 开放 `/home`，但仍隐藏 `/root` 和 `/run/user`。受管的 `sbin`、`share`、`html`、`modules`、`versions` 目录不得是符号链接。user 模式仍可安装在用户主目录。

自定义 system 根目录的实例前缀仍为 `P=R` 或 `R/instances/I`；程序、版本库、模块、静态文件、配置、日志、缓存和 agent 状态分别位于 `R/sbin`、`R/versions`、`R/modules`、`R/share`、`R/html`、`P/conf`、`P/logs`、`P/cache`、`P/agent`。运行目录继续使用 `/run/B` 与 `/run/B-I`，agent、wrapper、instance、sudoers 和 systemd 单元仍位于系统固定路径。

只允许首次安装时设置根目录。完整节点包可执行 `install.sh --prefix R`；RPM/DEB 应在安装前写入标记文件，软件包会把程序静态文件同步到 `R`。已有标记或已有默认安装时，传入不同根目录会拒绝且不迁移数据。

**user 模式**（安装根目录为 `R`，所有内容都在 `R` 之下，归安装用户所有）

| 内容 | 默认实例 | 具名实例 `I` |
| --- | --- | --- |
| 实例前缀 `P` | `R` | `R/instances/I` |
| `P/run`、`P/logs`、`P/cache` | 实际目录 | 实际目录 |
| agent 状态目录 | `P/agent` | `P/agent` |
| 程序 | `R/sbin/B`、`R/versions/`、`R/modules/`、`R/bin/B-agent`、`R/bin/B-wrapper`、`R/bin/B-instance` | 共用 |
| 服务 | `systemd --user` 单元（`~/.config/systemd/user/`），不可用时由 cron 兜底（见 6.3 节） | |

### 3.4 实例档案

实例的端口不属于布局，而是业务属性，记录在**实例档案**中：

```json
{
  "instance": "blue",
  "mode": "system",
  "root": "/opt/syedge",
  "http_port": 18080,
  "https_port": 18443,
  "http3": false,
  "status_port": 8082,
  "nofile": 65535
}
```

- 本机副本：`P/instance.json`，由 `B-instance add` 写入，agent 启动时读取。
- 控制台副本：**节点组**的一个属性（见 5.2 节）。

## 4. 品牌定制（R3）

### 4.1 为什么只能在构建时定制

数据面的名字是编译进二进制的：进程标题前缀（`SyEdge: master process`）、`-v` 输出、错误日志前缀、`Server` 响应头、默认错误页、HTTP/2 头的 HPACK 编码、热升级时传递监听套接字用的环境变量名，都是 patch 里的字符串常量。安装后只改文件名，只会影响 `ps` 的 comm 列，进程标题、日志和响应头仍然是旧名字。所以**品牌在构建时确定，一个安装包对应一个品牌**。发布流程为每个品牌产出一套安装包和升级包。

### 4.2 品牌参数

仓库根目录新增 `brand.env`，默认值等于当前产品：

| 参数 | 默认 | 示例 | 用途与约束 |
| --- | --- | --- | --- |
| `BRAND_ID` | `syedge` | `syedge` | 命令名、二进制名、路径、用户名、服务单元名、安装包名、环境变量前缀（大写形式）。约束为 `^[a-z][a-z0-9]{2,6}$`：小写便于命令行使用，长度上限 7 能保证 `B-console`、`B-agent-I` 这类名字在进程 comm 的 15 字符限制内尽量完整显示 |
| `BRAND_PROC` | 同 `BRAND_ID` | `SyEdge` | 进程标题前缀（`ps` 中显示为 `SyEdge: master process`）、错误日志前缀、`-v` 输出、`Server` 头、默认错误页。允许大小写字母和数字，长度不超过 15 |
| `BRAND_PRODUCT` | `SyEdge` | `SyEdge 边缘网关` | 控制台、关于页、安装提示中显示的产品名 |

### 4.3 数据面

- `dataplane/patches/0001-syedge-rename.patch` 改为模板 `0001-rename.patch.tmpl`，由 `dataplane/gen-rename-patch` 根据品牌参数生成实际的 patch：
  - 字符串常量中的名字被替换为 `BRAND_PROC` 或 `BRAND_ID`。
  - 带长度的常量（例如 `ngx_cpymem(errstr, "SyEdge: ", 8)`）的长度值由生成器按新名字计算。
  - HTTP/2 `server` 头的 HPACK 霍夫曼编码字节由现有的 `dataplane/hpack-gen` 生成。
  - 生成的 patch 仍然**只改字符串常量和宏的值**，ADR 0001 的约束不变。默认品牌生成的 patch 必须与现有 patch 逐字节相同，这一点作为测试。
- `build-syedge.sh -b brand.env` 根据品牌设置 `--prefix`、`--sbin-path`、`--user` 等编译参数。
- `rename-scan.sh` 和 `golden-rename.sh` 接收品牌参数。非默认品牌构建时，额外扫描二进制、安装包和运行日志，确认其中没有残留的 `syedge`（品牌泄漏扫描）。上游名 `nginx` 的检查保持不变。

### 4.4 Go 程序与打包

- 新增 `internal/brand`，变量通过 `-ldflags -X` 注入，默认值为 `syedge`。新增测试 `TestNoBrandLiteral`（与 `TestNoNetworkCode` 同类）：`internal/brand` 以外的非测试 Go 代码里不允许出现字面量 `syedge`。
- systemd 单元、sudoers、logrotate、tmpfiles、nftables、nfpm 配置都改为模板，由 `scripts/release/package.sh` 按品牌生成。
- 前端只构建一次。产品名、数据面名、命令名都通过 `/api/v1/about` 在运行时获取，不同品牌不需要重新构建前端。
- 控制台、agent、升级包都带上品牌标识：agent 注册时上报品牌，与控制台品牌不一致则拒绝注册；升级包的签名元数据包含品牌，wrapper 拒绝安装其他品牌的升级包。
- 新增本地目标 `make brand-matrix`：用一个测试品牌（例如 `sytest`）完整构建并跑黄金用例和泄漏扫描，证明没有写死的名字。按项目约定，这个检查在本地运行，不放进 Actions 自动触发。
- **实现说明（P11-D）**：定制品牌以 `-tags brand` 构建，默认品牌值只在 `internal/brand/default.go`（`!brand`）中，因此定制品牌的二进制不残留默认名（`-X` 只替换变量的值，不删除原来的初始值）。随包文档以 `@ID@` 占位符嵌入。Go 模块路径、协议与数据库字段名属于内部标识，保留不变，泄漏扫描对它们放行，详见 `docs/brand.md`“已知限制”。

### 4.5 已有安装

更换品牌等于更换产品：路径、用户、单元名全都不同，不支持原地改名。迁移方式是在新品牌下新装一套，再用现有的配置导入功能（P7）把配置迁过去。

## 5. 同机多实例（R1）

### 5.1 原则

1. **同组同档案**：同一个节点组内的所有节点使用相同的实例档案（实例名、模式、根目录、端口）。由此保证同组节点拿到的配置包逐字节相同，发布、灰度、漂移检测的现有逻辑都不需要改。
2. **同机不同组**：同一台机器上的多个实例必然属于不同的节点组。控制台以“主机”为单位检查实例名和端口是否冲突。
3. **二进制按主机共用，升级以主机为单位**（见 5.6 节）。
4. **默认实例不变**：没有具名实例的机器，行为与现在一致，现有节点无需迁移。

### 5.2 控制台模型

- `NodeGroup` 新增 `InstanceProfile` 字段（JSON，内容见 3.4 节）。默认值为 `{"instance":"", "mode":"system", "http_port":80, "https_port":443, "status_port":8081}`，与现状一致。
- `Node` 新增 `HostKey`、`Instance`、`Mode`、`Prefix` 字段。`HostKey` 是 `/etc/machine-id` 的 sha256，agent 已经读取这个文件生成机器指纹。所有改动通过新增的 gormigrate 迁移完成，不修改已冻结的迁移。
- 注册时校验：
  - agent 上报的实例名、模式、根目录和端口必须与节点组档案一致，否则拒绝，并在界面上给出原因。
  - 同一 `HostKey` 下，实例名不能重复；HTTP、HTTPS、HTTP/3（UDP）、状态页以及四层代理的监听端口不能与其他实例冲突。
- 修改节点组档案：端口可以改，保存前对组内所有节点所在的主机检查冲突，保存后提示需要发布；实例名、模式和根目录在组内已有节点后不允许修改。
- 四层代理的端口在保存时同样按主机做冲突检查。
- ACME HTTP-01 验证只能在 `http_port` 为 80 时使用，其他端口的节点组在界面上禁用 HTTP-01，改用上传证书。
- 界面：节点列表可以按主机折叠显示；节点组设置页新增“实例档案”区域。
- 授权：节点配额按实例计数还是按主机计数，属于商务决策，见第 11 节 D1。

### 5.3 渲染器

- `render.Input` 新增 `Profile`，取值来自节点组档案；`Profile` 为空时使用默认值。
- 模板按 3.2 节从 `Profile` 取路径；监听端口、状态页端口、HTTP/3 端口取自 `Profile`；不输出 `user` 指令（D5：master 本身以数据面用户运行，见 6.7 节）；`worker_rlimit_nofile` 取自档案的 `nofile`。
- 国密证书路径和 `render.ReleasesDir` 改为根据 `Profile` 计算。
- 默认档案的输出与现在逐字节相同，现有黄金用例不变。新增具名实例和 user 模式两组用例，按 `CLAUDE.md` 的要求通过真实的 `B -t`。

### 5.4 agent

- 启动参数精简为 `B-agent run [-instance I] [-root R]`，其余全部由布局和 `P/instance.json` 计算得出。删除写死的 PID、日志、状态地址和 wrapper 路径，同时修正 9b0716a 单元文件中不存在的参数。
- 漂移检测、漂移 diff 上报的目录限制改为 `P/conf/releases/`，单文件大小上限和私钥只报哈希的规则不变。
- 发布后的探活端口取自档案。
- 注册时上报品牌、实例名、模式、前缀、`HostKey` 和档案。
- agent 自升级（实现时修订）：新版本安装在各实例自己的 agent 状态目录（`<状态目录>/agent/<版本>/`），启动时验签后 exec，`/usr/bin/B-agent` 本身不被替换。因此各实例的 agent 自升级互不影响，不需要“发现共用二进制被替换后自行退出”的机制。agent 重启不会影响数据面。

### 5.5 wrapper 与 sudoers（新契约）

命令格式改为：

```
B-wrapper [--instance I] <test rN|reload|reopen|quit|upgrade-start V|upgrade-commit|upgrade-abort>
```

- 子命令和参数校验规则不变。`--instance` 只能是第一个参数，取值必须匹配实例名规则，并且对应的实例必须已经由 `B-instance add` 创建：前缀目录存在、归 root 所有、包含 `instance.json`。所有路径由 wrapper 根据布局计算，不接受外部传入的路径。
- 具名实例的 reload、reopen、quit 用 `B -c <实例的 current 配置> -s <信号>`。由于实例配置中的 `pid` 已按档案渲染为本实例的路径，syedge 会找到正确的进程，这样就修复了信号发到默认实例的问题。
- **每个具名实例使用独立的 agent 用户 `B-agent-I` 和独立的 sudoers 文件** `/etc/sudoers.d/B-agent-I`，由 `B-instance add` 生成，写入前用 `visudo -cf` 校验。文件内容固定 7 行，实例名直接写死：

  ```
  B-agent-blue ALL=(root) NOPASSWD: /usr/libexec/B/B-wrapper --instance blue test *
  B-agent-blue ALL=(root) NOPASSWD: /usr/libexec/B/B-wrapper --instance blue reload
  ...（其余 5 行同理，只有 test 和 upgrade-start 带参数通配）
  ```

  效果是：`agent@blue` 即使被攻破，也无法操作 green 或默认实例；实例之间的 agent 私钥也相互隔离。默认实例的 7 行 sudoers 保持不变。**删除** 9b0716a 加入的 `--instance *` 通配行。
- 注入测试矩阵：7 个子命令 × {无实例、合法实例、非法实例名、不存在的实例、`--instance` 不在第一位、重复的 `--instance`}，每一种都要有测试用例。

### 5.6 热升级（按主机）

数据面二进制（system 模式为 `/opt/B/sbin/B`，user 模式为 `R/sbin/B`）是主机级的软链，多个实例共用。升级规则：

1. 控制台制定升级计划时，以**主机**为最小单位分批。同一主机上的所有实例在同一批内依次升级，不允许只升其中一部分。
2. 主机内的顺序：第一个实例执行 `upgrade-start V`。wrapper 在主机锁（`versions/.upgrade.lock`，9b0716a 已实现）的保护下验签、安装版本、切换共用软链，再向该实例的 master 发送 USR2 并等待新进程就绪，然后执行 `upgrade-commit`。之后的实例执行 `upgrade-start V` 时，版本已经安装、软链已经切换，只需要对各自的 master 执行 USR2 和 commit。
3. 升级状态按实例保存（实现时修订）：`versions/state.json`（默认实例，与已部署节点兼容）与 `versions/state-<I>.json`，root 所有。wrapper 用 `/proc/<master>/exe` 判断本实例实际运行的版本：软链已指向目标版本、本实例却仍在运行旧版本时（同机前一个实例已完成升级），只给本实例的 master 换版本，状态中记 `keep_link`，中止时不切回软链；同一实例重复升级到正在运行的版本被拒绝。没有另设主机级状态文件：软链本身就是主机级状态，只由“切换它的那个实例”在中止时切回。
4. 失败处理：
   - 第一个实例在 commit 之前失败，执行 abort：该实例回到旧 master，共用软链也切回旧版本。
   - 第一个实例已经 commit，后续某个实例失败：该实例 abort，回到旧 master，继续运行内存中的旧二进制。控制台把该主机标记为“部分升级”，并提供两个选项：“重试剩余实例”，或者“主机整体回退”（对已升级的实例按同样的流程升级到上一个版本）。
   - 无论哪种情况，都不会停止或 restart 任何实例。
5. 控制台显示各实例实际运行的版本，同一主机上出现不同版本时给出告警（告警规则 `host_version_mixed`）。实现时修订：system 模式下 agent 与 root 的 master 不是同一用户，读不了 `/proc/<pid>/exe`；改为状态服务（只监听 127.0.0.1）增加 `/__B_version`，由 worker 返回编译时的版本号（`$B_version`），agent 在 Hello 与心跳中上报 `running_version`。它是上游版本号（不含发行序号），用于发现“同机实例运行不同版本”。
6. 控制台编排（`deploy.StartUpgrade`）：升级某个节点组时，把这些节点所在主机上其他节点组的实例一并纳入，涉及的节点组都加锁；主机按“先 1 台、再约 10%、其余”分批，批内主机并行，主机内本组实例在前、依次 START→COMMIT。已开始的主机会把实例做完，尚未开始的主机在出现失败后跳过。升级记录给出每台主机的结果（已升级 / 部分升级 / 保持原版本 / 未开始），“重试未完成的实例”（`POST /upgrades/:id/retry`）与“主机整体回退”（`POST /upgrades/:id/rollback-hosts`）都生成新的升级记录。

不采用“每个实例一份独立二进制”的方案。它虽然更简单，但会让每台机器的版本数量随实例数增长，Lua 附加包也要按实例重复安装，而且客户运维更难弄清每个实例用的是哪个版本。

### 5.7 实例管理命令 `B-instance`

使用 Go 实现（`cmd/B-instance`），便于测试；外部命令一律用 `exec.Command` 切片调用，不经过 shell。在 system 模式下由 root 执行，在 user 模式下由安装用户执行。

| 命令 | 行为 |
| --- | --- |
| `add I --http 18080 --https 18443 --status 8082 [--console 地址 --token 令牌 --ca-fingerprint 指纹]` | 校验实例名，检查端口未被占用（读取 `/proc/net/tcp*`、`/proc/net/udp*`）且不与其他实例的档案冲突；创建目录、软链、agent 用户、sudoers（system 模式）、服务单元；写入 `instance.json`；用 agent 的独立模式渲染器生成基线配置，执行 `B -t` 校验；启动数据面；如果带了注册参数，就注册并启动 agent |
| `remove I [--purge] --yes` | 停止 agent；对该实例执行优雅退出（`-s quit`，等待连接排空，超时可配置）；停用并删除单元、sudoers 和 agent 用户；删除配置（加 `--purge` 时同时删除日志和缓存）；最后提示在控制台中删除该节点。没有 `--yes` 时只打印将要执行的操作 |
| `list` | 列出每个实例的名称、前缀、端口、master 是否存活、实际运行版本、agent 状态 |
| `check` | 检查所有实例的端口冲突、目录权限和软链是否完整，供巡检和安装前预检使用 |
| `reopen-all` | 依次让所有实例重新打开日志，供 logrotate 调用 |
| `ensure`（user 模式） | 启动没有在运行的实例和 agent，不会重启正在运行的进程，供 cron 兜底调用 |

安装包和整机卸载：

- 安装包升级（`rpm -U`、`dpkg -i` 新版本）不停止任何实例。
- 卸载安装包（`rpm -e`）时，如果还存在具名实例，preremove 脚本直接拒绝，提示先执行 `B-instance remove`；设置环境变量 `B_FORCE_REMOVE=1` 可以强制卸载。
- `uninstall.sh` 增加 `--instance I`，作用等同于 `B-instance remove`。不带参数时，如果存在具名实例就拒绝执行，加 `--all` 才会卸载全部实例。

### 5.8 日志、防火墙与开机目录

- logrotate：匹配 `/var/log/B/*` 和 `/var/log/B-*/*`，`postrotate` 中调用 `B-instance reopen-all`。
- tmpfiles：开机时为每个已存在的实例创建 `/run/B-I`。具名实例的服务单元同时设置 `RuntimeDirectory=`，作为双重保障。
- nftables 防火墙：`B-firewall` 按全部实例当前生效配置中的 `listen` 指令（不含回环地址；`udp` / `quic` 为 UDP）生成放行规则，不再写死 80/443；读不到任何实例配置时回到 80/443。实例增删后由 `B-instance` 刷新规则，并维护 `B-firewall-refresh.path` 的附加配置，使每次发布（切换各实例的 `current`）后也自动刷新。某个实例监听 SSH 端口时拒绝生成、保留现有规则。

## 6. 非 root 运行（R2）

### 6.1 两种方式

| 方式 | 安装是否需要 root | 运行是否有 root 进程 | 适用场景 |
| --- | --- | --- | --- |
| **A. user 模式** | 不需要 | 没有 | 客户只给普通账号的服务器 |
| **B. system 模式（D5：默认且唯一的 system 运行方式）** | 只在装包时需要 | 没有：master、worker 与 wrapper 都以 `B` 用户运行，只带 `CAP_NET_BIND_SERVICE` | 有管理员装包的服务器 |

方式 B 的实现见 6.7 节。下面 6.2～6.5 节描述方式 A。

### 6.2 user 模式的安装与运行

- 安装：使用免安装包 `<前缀>-node-user-<版本>-linux-<架构>.tar.gz`，执行 `./install-user.sh ~/syedge --http 8080 --https 8443 --status 8081`，全程不需要 sudo。它把程序复制到 `R/bin`、`R/sbin`、`R/modules`，再执行 `R/bin/B-instance --root R init …` 创建默认实例。之后所有 `B-instance` 命令都带 `--root R`（带 `--root` 时拒绝以 root 运行；不带时仍要求 root）。
- 端口：普通用户只能监听 1024 及以上的端口（除非管理员调低了 `net.ipv4.ip_unprivileged_port_start`）。`B-instance add` 在安装时检查这一点；控制台对 user 模式的节点组禁止设置小于 1024 的端口。80/443 通常由前置的负载均衡转发进来。
- 数据面以安装用户身份运行。模板不输出 `user` 指令（非 root 的 master 会忽略该指令并打印警告）。`worker_rlimit_nofile` 取自档案，`B-instance add` 读取当前用户的硬限制作为上限。
- agent 与数据面使用同一个用户，**直接调用 wrapper，不经过 sudo**。wrapper 仍然执行同样的参数校验和升级包验签，相当于没有提权的场景下仍保留“受控入口”，控制台被攻破时也无法让节点执行任意命令或安装未签名的二进制。在 user 模式下，wrapper 如果发现自己以 root 身份运行，会拒绝执行，避免两种模式混用。
- 热升级：流程与 system 模式相同，版本库位于 `R/versions/`。

### 6.3 开机自启与守护

1. 首选 `systemd --user`：`B-instance add` 在 `~/.config/systemd/user/` 下为每个实例写入 `B[-I].service` 和 `B-agent[-I].service`（不用模板单元，因为一个用户可以有多个安装根目录）。要做到开机不登录就启动，还需要开启 linger（`loginctl enable-linger`）。很多发行版允许普通用户为自己开启 linger，但也有的需要管理员授权，所以安装程序会检测并提示。
2. 如果没有 `systemd --user` 或无法开启 linger，则使用 cron 兜底：写入 `@reboot B-instance ensure`，并每分钟执行一次 `B-instance ensure`。`ensure` 只启动未运行的进程，绝不重启正在运行的数据面。判断进程是否在运行时核对 `/proc/<pid>/exe`，重启后被复用的 pid 不会被当成数据面，也不会收到信号。

### 6.4 user 模式下不可用或替代的功能

| 功能 | 处理 |
| --- | --- |
| nftables 防火墙 | 不可用，安装时提示由主机管理员或安全组负责 |
| 系统 logrotate | agent 内置日志轮转：按天或按大小重命名日志，再通过 wrapper 执行 reopen，保留份数可配置 |
| ACME HTTP-01 | 只有能监听 80 端口时才可用，否则使用上传证书 |
| 小于 1024 的端口 | 不可用（除非管理员调低系统参数，或使用方式 B） |

### 6.5 安全权衡

user 模式下，agent 和数据面 worker 是同一个用户。worker 一旦被攻破（例如出现可利用的漏洞），攻击者就能读到 agent 的 mTLS 私钥，冒充这个节点连接控制台。缓解措施：控制台按证书识别节点身份，可以吊销单个节点；这个节点能拿到的也只是本组的配置，而配置里不包含其他节点的私钥。部署手册中会说明：面向互联网的生产节点仍然推荐 system 模式（agent 与 worker 用户隔离），user 模式适用于无法获得 root 的环境。

### 6.7 system 模式运行时不使用 root（方式 B，D5，已实现）

安装、卸载与 `B-instance` 等管理命令由管理员以 root 执行；运行时的进程都不是 root：

| 进程 | 身份 | 能力 |
| --- | --- | --- |
| 数据面 master 与 worker（`B.service`、`B@I.service`） | `B` | `AmbientCapabilities=CAP_NET_BIND_SERVICE`（`CapabilityBoundingSet` 只留这一项，`NoNewPrivileges=yes`）。ambient 能力跨 exec 保留，热升级（USR2 重新执行）产生的新 master 同样能绑定 80/443 |
| wrapper（agent 经 sudo 调用） | `B`（sudoers 为 `(B)`，7 行不变） | 文件能力 `cap_net_bind_service=ip`（只在 permitted / inheritable，不在 effective），文件属组 `B`、权限 0750。`-t` 会绑定配置中的端口，wrapper 把这一项设为 ambient 后再执行 `B -t`。wrapper 以 root 运行时拒绝执行 |
| agent | `B-agent[-I]` | 无 |
| 防火墙开机加载、发布后刷新（`B-firewall*.service`） | `B-firewall` | 只有 `CAP_NET_ADMIN`；规则与参数在 `/var/lib/B-firewall/`，归该用户 |
| logrotate 的 reopen | `B`（`su B B`，`runuser -u B`） | 无 |

- 模板不再输出 `user` 指令（worker 与 master 同一用户）。
- 运行时目录、日志、缓存与版本库归 `B`；二进制所在目录与模块目录本身归 `B`（wrapper 在其中切换软链、安装新版本），已安装的文件仍归 root。私钥由 agent 写为 0640、组 `B`，master 可读。
- **已以 root 运行的 master**（升级前启动）不能 restart，由 `B-instance nonroot`（安装与升级软件包时自动执行，可重复执行）无中断切换：共用软链临时指向 root 所有的 `B-instance`（launcher），USR2 → 旧 master exec launcher → launcher 读取 root 所有的 `versions/.handover.json`，降权为 `B`、只保留 `CAP_NET_BIND_SERVICE`（ambient），exec 真正的二进制；监听套接字随 exec 继承 → 软链立即切回 → WINCH / QUIT 旧 master。新 master 身份或能力不对时让它退出，旧 master 照常服务。`nonroot` 同时交出目录、重写具名实例的 sudoers、设置 wrapper 的文件能力、重启 agent（只重启 agent）。
- 安全权衡：master 与 worker 同一用户，二进制目录与版本库对 `B` 可写。worker 被攻破时可以替换二进制或读取私钥（与以前 worker 能读私钥相同），但没有任何 root 进程可被利用，替换后的二进制也只以 `B` 身份、带 `CAP_NET_BIND_SERVICE` 运行。升级包仍须厂商签名才会被 wrapper 安装。


**兼容模式（2026-11-07 决定：与 nginx 保持一致）**：ambient 能力需要内核 4.3 与 systemd 229 起，CentOS 7（内核 3.10、systemd 219）不支持。
`B-instance nonroot` 检测到不支持（`/proc/self/status` 没有 `CapAmb`，或 `systemctl --version` < 229）时，或指定 `--root-master` 时，
写入 root 所有的标记 `/etc/B/root-master`，并由软件包单元生成完整的覆盖单元 `/etc/systemd/system/B.service` 等（`User=root`，只有一行边界集，列出 root master 用到的能力；不用附加配置：systemd 229 以下多行边界集取交集，无法放宽软件包单元，2026-11-07 在 systemd 208 上发现），
sudoers 改为 `(root)`（7 行不变），agent 以 `sudo -n` 调用 wrapper，wrapper 只在标记可信（root 所有、他人不可写）时接受以 root 运行。
master 以 root 绑定端口，worker 按编译进二进制的默认用户（`--user=B`）运行，与 nginx 相同；热升级、多实例与日志轮转的流程不变。
nonroot 只写配置、不停止也不重启数据面；验收：`scripts/ci/p11-acceptance.sh` 的 root-master 阶段。

### 6.6 控制台的非 root 运行

控制台本来就以 `syedge-console` 普通用户运行，端口为 8443 和 9443，数据目录可以通过 `-data` 指定。这里只需要补充：免安装包、`systemd --user` 单元示例，以及授权后台的同类说明。

## 7. 安全分析

| 风险 | 措施 |
| --- | --- |
| 某个实例的 agent 被攻破后操作其他实例 | 每个实例使用独立的 agent 用户和独立的 sudoers，实例名写死在 sudoers 中 |
| 通过实例名注入路径 | 实例名格式严格限定，路径全部由 wrapper 自行计算，实例必须已由 root 创建 |
| 共用二进制被某个实例的流程替换成未签名的版本 | 只有 wrapper 能切换软链，必须经过厂商签名校验；升级包签名中包含品牌标识 |
| 并发升级破坏共用版本库 | 主机锁加上拆分后的主机级 / 实例级升级状态 |
| 实例删除时影响其他实例 | `remove` 只操作该实例自己的前缀、单元、用户和 sudoers；共用文件只在卸载安装包时处理，并且有具名实例时拒绝卸载 |
| user 模式下同一用户导致隔离减弱 | 见 6.5 节，在文档中明确说明适用边界 |
| 运行时的 root 进程被利用 | 运行时没有 root 进程（6.7 节）：数据面与 wrapper 只有 `CAP_NET_BIND_SERVICE`，防火墙服务只有 `CAP_NET_ADMIN` |
| 品牌包混用 | 注册和升级时都校验品牌标识 |

## 8. 兼容与迁移

- 已部署的单实例节点（v0.1 / v0.2）：默认档案的渲染结果与现在逐字节相同，路径、配置、数据面进程都不变，不需要任何迁移步骤。
- 当前的 wrapper 调用方式（不带 `--instance`）继续有效。

## 9. 需要修改的 `CLAUDE.md` 条款

评审通过后，这些条款与实现在同一个 PR 中修改：

1. “`--conf-path=/opt/syedge/conf/current/syedge.conf`” 补充：这是默认实例的配置路径；具名实例以 `-c <实例前缀>/conf/current/<B>.conf` 启动，配置中的绝对路径一律由渲染器按节点组档案生成。
2. wrapper 的命令格式增加 `[--instance I]` 前缀；sudoers 规定为：默认实例 7 行，每个具名实例一个独立文件，同样 7 行，实例名写死，禁止对实例名使用通配。
3. 漂移 diff 上报的目录限制，从固定的 `/opt/syedge/conf/releases/` 改为“本实例前缀下的 `conf/releases/`”。
4. “不得停止 syedge”增加一条例外：只有运维人员显式执行的 `B-instance remove` 和卸载安装包，可以对该实例执行优雅退出。
5. 品牌：除 `internal/brand` 和构建默认值以外，代码中不允许出现 `syedge` 字面量；非默认品牌的构建必须通过品牌泄漏扫描。
6. user 模式：wrapper 直接调用、不经过 sudo，但校验和验签规则不变。

## 10. 实施计划

### 10.1 立即处理（P11 开始之前，约半天）

针对 9b0716a，在客户环境启用多实例之前先做最小修复：

- 删除 sudoers 中的 `--instance *` 通配行。
- 从安装包中暂时移除 `syedge@.service` 和 `syedge-agent@.service`，等 P11-B 完成后再按本设计重新加入。
- 保留 9b0716a 中与多实例无关的部分：磁盘空间预检、发布并发上限、非交互卸载、发布门禁脚本。

### 10.2 分期

| 阶段 | 内容 | 预计（AI 编码，工作日） | 依赖 |
| --- | --- | --- | --- |
| P11-A 布局与品牌基础 | 新增 `internal/layout`、`internal/brand`；Go 代码改为调用这两个包；`TestNoBrandLiteral`（PR #25）。渲染器接收实例档案并按档案渲染路径和端口；默认档案输出不变；具名实例与 user 模式的输出通过真实 `B -t`（PR #26） | 3–4 | — |
| P11-B 多实例（system 模式） | 实例档案（控制台、渲染器、agent）；新的 wrapper 和 sudoers 契约及注入测试；主机级升级编排与状态拆分；`B-instance`；logrotate、tmpfiles、防火墙；安装包卸载保护 | 5–6 | A |
| P11-C 非 root 运行 | user 模式布局、免安装包、`systemd --user` 和 cron 兜底、agent 内置日志轮转；方式 B（降权运行）开关 | 3–4 | A，可与 B 并行 |
| P11-D 品牌参数化构建 | patch 模板与生成器（长度计算、HPACK）；构建、打包、单元文件模板化；前端运行时获取品牌；注册与升级包的品牌校验；`make brand-matrix` 与泄漏扫描 | 4–5 | A，可与 B、C 并行 |
| 验收 | 第 10.3 节全部用例，包括麒麟 V10 和统信 UOS 实机 | 2 | B、C、D |

### 10.3 验收用例

| 编号 | 场景 | 通过标准 |
| --- | --- | --- |
| MI-01 | 同一台机器上运行默认实例、blue、green，端口各不相同，分别发布和 reload | 每次只有目标实例的 worker 被替换，其他实例的 master PID、worker PID、配置版本都不变 |
| MI-02 | 端口冲突（注册时、修改档案时、新增四层代理时、`B-instance add` 时） | 全部被拒绝，并给出冲突的实例和端口 |
| MI-03 | 3 个实例的主机级热升级，持续压测 | 所有实例都切换到新版本，压测零错误；在第二个实例上注入失败时，按 5.6 节的规则处理，任何实例都没有停止 |
| MI-04 | 删除 blue | green 和默认实例持续服务，压测零错误；有具名实例时，卸载安装包被拒绝 |
| MI-05 | 以 `B-agent-blue` 用户操作 green | sudo 拒绝 |
| MI-06 | wrapper 注入测试矩阵（5.5 节） | 全部返回用法错误，不执行任何命令 |
| MI-07 | 日志轮转 | 每个实例都重新打开了自己的日志文件 |
| RL-01 | 在没有安装 sudo 的容器里，以 uid 1000 完成安装、注册、发布、热升级、卸载 | 全部成功 |
| RL-02 | 运行中检查进程属主 | 没有属于 root 的进程 |
| RL-03 | 重启机器 | 通过 linger 或 cron 自动恢复所有实例 |
| BR-01 | 以 `syedge` / `SyEdge` 品牌构建并安装 | `ps` 显示 `SyEdge: master process`；`syedge -t`、`systemctl reload syedge@blue` 可用；`Server` 头和错误页显示 SyEdge；路径、单元名、包名都是 syedge |
| BR-02 | 品牌泄漏扫描 | 二进制、安装包、界面、日志中没有 `syedge`，也没有上游名 |
| BR-03 | 品牌不一致的 agent 注册，或安装其他品牌的升级包 | 被拒绝 |
| BR-04 | 默认品牌构建 | 生成的 patch 与现有 patch 逐字节相同，黄金用例全部通过 |

## 11. 待决策事项

| 编号 | 问题 | 建议 |
| --- | --- | --- |
| D1 | 授权的节点配额按实例计数，还是按主机计数 | 按实例计数。模型简单，与“每个实例都是一个受管节点”一致；如果商务上希望同机多实例更便宜，可以在授权中另加“每主机实例上限” |
| D2 | 品牌名的大小写：命令和路径用小写 `syedge`，进程标题和 Server 头用 `SyEdge` | 按此执行。如果希望命令也是 `SyEdge`，技术上可行，但不符合 Linux 命令和路径的惯例 |
| D3 | 授权文件是否绑定品牌，防止某个 OEM 品牌的授权被用到另一个品牌的安装包上 | 绑定。授权中增加可选的品牌字段，不填时视为通用（已实现：`product.brand`，签发时 `-brand`） |
| D4 | user 模式是否必须支持没有 `systemd --user` 的机器 | 必须支持。cron 兜底的实现成本低，而且国产操作系统上经常无法开启 linger |
| D5 | 是否需要方式 B（system 模式运行时不使用 root） | 需要（已确认并实现，见 6.7 节）：作为 system 模式的默认且唯一的运行方式；已以 root 运行的 master 由 `B-instance nonroot` 无中断切换 |
| D6 | v0.2 是否等 P11 完成 | 不等。v0.2 只包含 10.1 节的最小修复，按单实例交付试点；P11 进入 v0.3 |
| D7 | `Server` 响应头与错误页显示什么 | 显示品牌的显示名：默认品牌为 `SyEdge`（2026-11-06 起产品更名为 SyEdge；此前默认品牌为 syapp），定制品牌为其显示名，不隐藏。由改名 patch 模板按品牌生成：HTTP/1.1、HTTP/2（HPACK 编码）、HTTP/3 的 Server 头与默认错误页都用显示名；`server_tokens off` 时不带版本号。实现时修复了两处：HTTP/2 的 HPACK 字节曾写死为默认品牌，控制台输出的前缀长度曾写死为默认长度（显示名长度不同时吞掉消息首字符）。由 `scripts/ci/brand-runtime.sh` 对默认品牌与定制品牌分别实测（CI：`dataplane`、`dataplane-brand`） |
