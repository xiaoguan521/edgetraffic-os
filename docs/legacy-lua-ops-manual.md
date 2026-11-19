# SyEdge 控制面导入后切换独立运行：Lua 兼容运维手册

> 适用对象：已经使用 SyEdge 控制面管理节点，准备导入现有 Nginx/Lua 配置，并在验证后切换为节点本地独立维护的场景。
>
> 本手册针对当前仓库中的 `demo/nginx.conf`、`demo/houtai.conf`、`demo/qiantai.conf` 和 `demo/lua/` 编写。生产执行前必须先在单节点或灰度节点演练。

## 1. 先给结论

推荐继续使用现有 SyEdge，安装与 SyEdge **完全同版本、同架构、同加密变体**的 `syedge-lua` 扩展，然后增加一个“独立运行/legacy Lua”兼容层。当前阶段不建议把整套 SyEdge 替换成 OpenResty。

原因如下：

1. SyEdge 已经包含 Nginx、LuaJIT、`ngx_http_lua_module`、NDK 和控制面/Agent/非 root 运行约束。替换成 OpenResty 会同时影响控制面下发、配置渲染、服务管理、升级、GMSSL/Tongsuo 变体和回滚。
2. 独立运行模式可以使用原生 `*_by_lua_file`、`require`、部分 cosocket 和第三方 Lua 库，但仍然必须满足 LuaJIT ABI、模块版本、路径和权限要求；安装 OpenResty 也不会自动提供 `cjson`、`resty.mysql` 或你们自己的脚本。
3. 如果最终目标是完全不受 SyEdge 运行时约束、直接复用完整 OpenResty 生态，建议将旧 Lua 网关作为独立 OpenResty sidecar/旧节点运行在 SyEdge 后面，而不是直接替换 SyEdge 主节点。

“控制面先导入”应理解为：先完成配置资产盘点、站点/上游/路由映射、版本留痕和灰度验证；**导入成功不等于 Lua 脚本已经可以在控制面托管模式执行**。当前产品的托管 Lua 是受限沙箱，旧脚本和第三方库通常需要放到独立运行路径中验证。

## 2. 两种运行模式的边界

| 模式 | 配置来源 | Lua 能力 | 适合用途 |
|---|---|---|---|
| 控制面托管 | 控制面生成 `/opt/syedge/conf/current`，Agent 持续接管 | 受限的自定义 Lua 规则；不能依赖任意 `dofile`、外部 `require`、`ngx.socket`、动态上游等能力 | 标准站点、认证、限流、重写、可审计规则 |
| 独立运行 | 节点本地维护的主配置和 `conf.d` | 在已加载 Lua 模块的前提下使用原生 Lua 指令、受控第三方库和 cosocket | 兼容现有 legacy Lua，逐步迁移或长期保留旧网关 |

独立运行的关键条件是：Agent 不再对该节点发布配置。仅仅在控制面导入文件、或者停止一次发布，不代表节点已经脱离控制面；Agent 仍运行时，后续同步可能覆盖本地配置。

## 3. 当前 demo 的上线前盘点

先在源码目录生成清单并保存到变更单：

```bash
cd /Users/youyou/Downloads/edge_traffic/edgetraffic-os

rg -n --hidden --glob '!*.bak' --glob '!*.lua20*' \
  'load_module|include|_by_lua|lua_package|proxy_pass|set \$' \
  demo/nginx.conf demo/houtai.conf demo/qiantai.conf demo/lua
```

当前三份配置中实际被引用的 Lua 主要是：

| 文件/引用 | 当前情况 | 上线动作 |
|---|---|---|
| `demo/nginx.conf:37` → `init_pt_route.lua` | 初始化 `addrTable`、`gatewayTable`；能在独立模式使用 | 确认 `lua` 相对路径、前缀和所有目标地址；逐条测试路由 |
| `demo/houtai.conf` 多处 → `ignoreGateway.lua` | 读取请求头和 `originalAddr/gatewayAddr`，写入 `targetAddr` | 独立模式可用；确认变量先 `set`，并禁止客户端通过请求头绕过安全网关 |
| `demo/houtai.conf:190`、`demo/qiantai.conf:676` → `kkfileOpenType.lua` | 脚本输出 JSON，但配置放在 access 阶段 | 改为 `content_by_lua_file`，或让脚本明确 `ngx.exit(ngx.OK)`；否则可能继续执行后续阶段 |
| 其余 `demo/lua/*.lua` | 多数没有被这三份配置引用 | 不要因为文件存在就加载；逐个登记用途、依赖、入口和回滚方式 |

必须先修复或确认的确定性问题：

- `demo/nginx.conf:85-86` 仍引用 `/home/atwasoft/openresty/nginx/conf/qiantai.conf` 和 `houtai.conf`。导入或独立运行时必须改成节点上的明确路径，不能依赖旧机器目录。
- `demo/houtai.conf:351` 的 `10.100.51.11:306311` 和 `:371` 的 `10.100.51.11:300061` 不是合法 TCP 端口。确认业务端口后再发布。
- `DynamicAddressesLua.lua` 使用 `require("cjson")` 和 `cjson.encode`。当前随 `syedge-lua` 打包的 `etos.json` 只有解码能力，不能直接替换 `cjson.encode`。
- `access_pt_route.lua`、`access_pt_jg_route.lua`、`jit/*.lua`、`bb.lua`、`ptcore*.lua` 等没有被当前三份配置接入，不能用“语法检查通过”代替功能验证。
- `jit/lua_new_access.lua` 依赖 `resty.mysql`、`cjson` 和未定义的 `/childrequest`、`body`，还存在 SQL 拼接风险。默认不纳入本次上线。

## 4. 阶段 A：控制面导入、审计和灰度发布

### 4.1 导入前准备

在控制面操作前，准备以下资产包和清单：

```text
legacy-gateway/
├── nginx.conf
├── houtai.conf
├── qiantai.conf
├── lua/                    # 只放确认要加载的活动脚本
├── lualib/                 # 第三方纯 Lua 库
├── static/                 # /home/atwasoft/html 等静态文件
├── certs/                  # 证书、私钥，单独按安全流程导入
└── MANIFEST.sha256         # 文件校验和、版本、来源、负责人
```

记录以下信息，后面作为回滚依据：

```bash
ROOT=/opt/syedge
SY="$ROOT/sbin/syedge"

sudo "$SY" -v
sudo "$SY" -V
uname -m
readlink -f "$ROOT/modules/lua" || true
systemctl status syedge syedge-agent --no-pager || true
```

同时确认：

- 目标节点的 SyEdge 版本、CPU 架构和 standard/GMSSL 变体。
- 控制面已经启用对应的 Lua 功能授权（如果托管 Lua 规则需要授权）。
- 目标节点有足够磁盘和备份空间，能够回滚配置、证书、静态文件和 Lua 依赖。
- 先选择一台可摘流量的灰度节点，禁止直接对全量节点发布。

### 4.2 导入方式

控制面中优先导入完整配置展开结果或配置包，并在导入报告中逐项处理：

1. 主配置、`include` 文件、站点和上游。
2. 静态目录、证书/私钥、WebSocket 和长连接参数。
3. Lua 脚本及其依赖清单；不要只上传 `.lua` 文件而遗漏库和资源文件。
4. 导入报告中的绝对路径、重复监听端口、未知指令、未知变量和缺失文件。

不要把 `/home/atwasoft/openresty/nginx/conf/...` 当成生产路径直接导入。应先将路径映射到目标节点的版本化目录，例如：

```text
/opt/syedge/legacy-lua/config/qiantai.conf
/opt/syedge/legacy-lua/config/houtai.conf
/opt/syedge/legacy-lua/lua/kkfileOpenType.lua
/opt/syedge/legacy-lua/lualib/resty/...
/opt/syedge/legacy-lua/static/...
```

导入完成后，先保存“草稿/导入报告”，不要立即覆盖所有生产节点。对控制面能原生表达的能力优先转换为站点、上游、重写、认证和限流规则；对动态上游、任意 `require`、数据库写入等能力，标记为“独立模式兼容项”，不要伪装成托管规则。

### 4.3 发布前检查和灰度

在控制面灰度发布前完成：

- 所有 `include`、静态目录和证书都存在，并且权限符合非 root 运行要求。
- 非法端口已经确认并修正。
- 每个 Lua 引用都有入口、依赖、责任人和回滚版本。
- 先发布不依赖 legacy Lua 的基础站点，确认控制面/Agent/回滚链路正常。
- 记录控制面版本号、节点版本号和 `syedge -T` 展开配置。

在节点上执行等价检查（命令路径以实际安装为准）：

```bash
sudo /opt/syedge/sbin/syedge -t
sudo /opt/syedge/sbin/syedge -T > /var/lib/syedge-agent/backups/managed-expanded-$(date +%F-%H%M%S).conf
sudo systemctl reload syedge
sudo journalctl -u syedge -n 200 --no-pager
```

这里的目标是证明控制面导入后的基础配置可运行，不是证明所有 legacy Lua 已经兼容。

## 5. 阶段 B：安装和维护 Lua 兼容层

### 5.1 安装同版本 `syedge-lua`

从与节点 SyEdge 版本匹配的发布物获取 `syedge-lua`。不要在生产节点直接把另一台 OpenResty 的 `.so`、LuaJIT 或 `lualib` 复制过来。

示例流程：

```bash
mkdir -p /tmp/syedge-lua-stage
tar -C /tmp/syedge-lua-stage -xzf syedge-lua-<syedge-version>-<arch>.tar.gz

# 默认实例；命名实例按实际实例名替换
sudo bash /tmp/syedge-lua-stage/install.sh --instance default

readlink -f /opt/syedge/modules/lua
ls -la "$(readlink -f /opt/syedge/modules/lua)"
sudo /opt/syedge/sbin/syedge -v
sudo /opt/syedge/sbin/syedge -V
```

安装包必须同时匹配：

- SyEdge 版本；
- CPU 架构；
- standard 或 GMSSL/Tongsuo 变体；
- 运行实例和安装前缀。

安装脚本自带的自检只证明 Lua 模块和基础 `etos` 库能被加载，不代表业务脚本、`cjson` 或 `resty.mysql` 已经可用。

### 5.2 目录约定

不要修改 `/opt/syedge/modules/lua/lualib` 作为业务脚本目录，因为升级或替换扩展包时该目录可能被覆盖。建议单独维护：

```text
/opt/syedge/legacy-lua/
├── config/                 # 独立模式主配置和 conf.d
├── lua/                    # 活动业务脚本
├── lualib/                 # 第三方纯 Lua 库
├── cmodules/               # 经批准、同 ABI 编译的 C Lua 模块
├── static/
├── certs/
├── manifest.sha256
└── releases/<release-id>/  # 每次发布一份不可变副本
```

权限建议：配置、脚本、纯 Lua 库对 `syedge` 可读但不可写；私钥仅授予必要的服务用户；发布由 root 或受控发布账号完成。脚本中不要写入数据库密码、Token 或私钥。

### 5.3 第三方库加载规则

在独立模式的 `http` 块中按实际目录设置路径，例如：

```nginx
lua_package_path "/opt/syedge/modules/lua/lualib/?.lua;/opt/syedge/legacy-lua/lualib/?.lua;/opt/syedge/legacy-lua/lua/?.lua;;";

# 只有确实安装了同 ABI 的 C 模块时才启用；纯 Lua 依赖不要打开它。
lua_package_cpath "/opt/syedge/legacy-lua/cmodules/?.so;;";
```

规则：

1. `lua-cjson` 必须针对当前 LuaJIT/模块 ABI 编译或使用已验证的同版本包；不能只把任意 `cjson.so` 改名复制。
2. `lua-resty-mysql` 属于 cosocket 场景，只在独立模式、已安装兼容版本且数据库网络已限制时使用；它不应成为每个请求的同步关键路径。
3. `etos.json` 目前不能替代 `cjson.encode`。若只是生成少量 JSON，优先使用静态预生成文件；若必须动态编码，选择经过验证的纯 Lua 编码器或同 ABI 的 `lua-cjson`。
4. 每个第三方库都登记来源、版本、SHA-256、许可证、LuaJIT 兼容性和回滚包。
5. 扩展包升级后必须重新执行 `require` 自检和业务回归；不能只看模块软链接仍然存在。

## 6. 阶段 C：把 demo 配置转换为可运行的独立配置

### 6.1 主配置加载模块

`load_module` 必须在主配置顶层、`events` 之前加载；不能把它放进 `server`、`location` 或普通控制面规则中。确认主配置包含与实际安装目录一致的路径，例如：

```nginx
load_module /opt/syedge/modules/lua/ndk_http_module.so;
load_module /opt/syedge/modules/lua/ngx_http_lua_module.so;
```

如果当前 SyEdge 生成模板已经根据 `HasLua` 加载模块，不要重复加载；用 `syedge -T` 检查最终展开配置。

### 6.2 文件和路径

将相对路径改成可审计的绝对路径，或明确设置 SyEdge 前缀。例如：

```nginx
init_by_lua_file /opt/syedge/legacy-lua/lua/init_pt_route.lua;
```

`nginx.conf` 中的：

```nginx
include /home/atwasoft/openresty/nginx/conf/qiantai.conf;
include /home/atwasoft/openresty/nginx/conf/houtai.conf;
```

必须替换为目标节点实际存在的路径，并将静态目录、证书目录一并迁移和校验。

### 6.3 变量声明和动态上游

Lua 写入的 Nginx 变量必须在对应作用域提前声明。例如 `ignoreGateway.lua` 当前依赖：

```nginx
set $originalAddr "";
set $gatewayAddr "";
set $targetAddr "";
```

当前 demo 的若干 location 已有类似声明，但必须在最终 `syedge -T` 中逐个确认。`access_pt_route.lua`、`jit/*.lua`、`ptcore*.lua` 还依赖 `$proxy_addr`、`$myhost`、`$ydname`、`$targetDMAddr` 等变量；未接入的脚本不要仅为“通过检查”而盲目添加变量。

对于：

```nginx
proxy_pass http://$targetAddr;
```

必须保证 `$targetAddr` 只能来自固定、审核过的上游地址表。不能让请求头、URI 或查询参数直接变成任意 `host:port`，否则会形成 SSRF 和网关绕过。特别是 `ignoreGateway.lua` 读取 `ignore-gateway` 请求头，只有在上游会剥离/校验该头、且业务确实信任它时才允许上线。

### 6.4 `kkfileOpenType.lua` 的阶段

该脚本本质上生成响应内容，建议将对应 location 改为：

```nginx
location = /kkfile40/checkFileType {
    content_by_lua_file /opt/syedge/legacy-lua/lua/kkfileOpenType.lua;
}
```

如果必须保留 `access_by_lua_file`，脚本必须在输出后明确结束请求，并处理缺失、重复和非法的 `fileType` 参数；否则 access 阶段输出后仍可能继续执行 rewrite/content/proxy 阶段。生产前至少测试：`doc`、`pdf`、未知类型、缺失参数、重复参数、编码异常。

### 6.5 路由表脚本

`init_pt_route.lua`、`access_pt_route.lua` 和 `access_pt_jg_route.lua` 不能只靠文件存在自动生效。要使用它们，必须同时具备：

- `init_by_lua_file` 已执行；
- 对应 location 绑定 `access_by_lua_file`；
- 写入的变量已经 `set`；
- `proxy_pass` 使用固定的目标集合；
- `request_uri` 的精确匹配已覆盖编码、查询参数和默认路由。

建议先把路由表静态化并对目标地址做启动时校验；不要直接把用户可控的完整 URI 拼成代理目标。

### 6.6 明确暂缓的脚本

以下脚本在本次切换中默认保持禁用，完成重构和单独审批后再启用：

- `DynamicAddressesLua.lua`：缺少可用的 `cjson.encode` 依赖。
- `jit/lua_new_access.lua`：依赖 `resty.mysql`/`cjson`，还有 SQL 拼接、未定义 body 和内部 location 问题。
- `bb.lua`：使用固定的 `/home/atwasoft/openresty/nginx/lua/...` 和全局变量，必须先改为版本化绝对路径。
- `jit/lua_access_childrequest.lua`：需修复布尔值判断、类型判断、响应头和内部子请求契约。
- `ptcoreDM.lua`、`ptcoreGLDX.lua`、`ptcoreORG.lua`、`ptcoreSXDY.lua`、`ptcoreSXGL.lua`、`ptcoreTBGL.lua`：先确认所有对应的 location、变量和目标上游确实存在。

## 7. 阶段 D：独立运行切换

### 7.1 首选：蓝绿或独立实例

最安全的方案是保留当前控制面节点作为回滚组，另起一台节点或一个独立实例：

1. 控制面节点完成导入和基线发布。
2. 新节点安装同版本 `syedge-lua`，放置版本化的 `legacy-lua` 目录。
3. 使用独立主配置加载 Lua 模块和 `conf.d`，完成语法、依赖、业务和压力验证。
4. 先将少量流量切到独立节点，观察错误率、P95/P99、连接数、Lua 错误和上游状态。
5. 确认稳定后再扩大流量；原控制面节点保留一段观察期。

这种方式不会把“脱离控制面”和“生产流量切换”绑定在同一个不可逆操作中。

### 7.2 同一节点切换

只有在无法蓝绿部署时采用。服务名和实例名以现场为准，先检查：

```bash
systemctl list-unit-files | rg 'syedge|agent'
systemctl status syedge-agent syedge --no-pager
```

切换顺序：

1. 在控制面将节点摘出发布组或置于维护/排空状态；记录最后一个成功版本。
2. 备份当前配置、展开配置、证书引用、静态目录清单、Lua 包和服务单元：

   ```bash
   sudo install -d -m 0700 /var/lib/syedge-agent/backups
   sudo tar -C /opt/syedge/conf/current \
     -czf /var/lib/syedge-agent/backups/standalone-manual-$(date +%F-%H%M%S).tar.gz .
   sudo chmod 0600 /var/lib/syedge-agent/backups/standalone-manual-*.tar.gz
   ```

3. 确认没有正在执行的控制面发布、自动升级或配置回滚。
4. 停止并禁用 Agent，防止它继续覆盖本地配置；**不要删除** `/var/lib/syedge-agent`、`node.json` 或审计数据：

   ```bash
   sudo systemctl stop syedge-agent
   sudo systemctl disable syedge-agent
   systemctl is-active syedge-agent || true
   ```

   如果现场使用的是命名实例或不同 unit，按实际 unit 执行同样动作。
5. 将已验证的独立配置放入版本化目录，再由 SyEdge 服务指向该目录。不要在 Agent 仍运行时直接编辑控制面生成目录。
6. 以 `syedge` 运行身份做最终检查，确认日志、缓存、运行目录和 Lua 文件可读；不要为了通过检查而把服务改成 root。
7. 执行语法检查，通过后只做 reload：

   ```bash
   sudo -u syedge /opt/syedge/sbin/syedge -t
   sudo -u syedge /opt/syedge/sbin/syedge -T \
     > /opt/syedge/legacy-lua/releases/<release-id>/expanded.conf
   sudo systemctl reload syedge
   ```

8. 观察至少一个完整业务高峰或约定的观察窗口；确认本地配置没有被重新生成、覆盖或标记为未授权变更。

停止/禁用 Agent 只是让节点本地维护配置，不等于从控制面删除节点。以后要重新接管时，先停独立流量和服务，再恢复 Agent；是否重新注册取决于控制面节点身份和令牌状态，不要直接删除节点数据来“强制解绑”。

## 8. 上线验证清单

### 8.1 进程、模块和依赖

```bash
readlink -f /opt/syedge/modules/lua
sudo /opt/syedge/sbin/syedge -t -q
sudo /opt/syedge/sbin/syedge -T | rg -n \
  'load_module|lua_package|_by_lua|proxy_pass|set \$|include'
sudo journalctl -u syedge -n 200 --no-pager | rg -i \
  'lua|module|not found|unknown directive|variable|error'
```

如果需要验证库加载，在独立配置中为每个库做一次最小 `require` 自检；不要把自检脚本长期挂在生产请求路径上。重点检查：

- `require("resty.core")`；
- `require("resty.lrucache")`；
- 业务实际使用的 `require("cjson")`；
- 业务实际使用的 `require("resty.mysql")`；
- 所有 `dofile`/`loadfile` 的绝对路径和权限。

### 8.2 业务冒烟

至少覆盖：

1. `/kkfile40/checkFileType?fileType=doc`、`pdf`、未知类型、缺失/重复参数，确认响应码、`Content-Type` 和 JSON 正确。
2. `ignore-gateway` 缺失、值为 `0`、其他值三种情况，确认实际代理目标；确认客户端无法构造任意目标地址。
3. `init_pt_route` 的已知路由、默认路由、网关路由和错误路由。
4. HTTPS、证书链、WebSocket、静态文件、超时、上传大文件和长连接。
5. 所有从 demo 中修正过的端口和上游健康检查。
6. 真实错误路径：Lua 抛错、依赖缺失、目标上游不可达时，返回码和日志不能泄露敏感信息。

冒烟期间持续观察：

```bash
sudo tail -F /var/log/syedge/error.log
sudo journalctl -u syedge -f
```

重点搜索 `module not found`、`attempt to index`、`variable ... not found for writing`、`unknown directive`、`no resolver defined`、`upstream timed out` 和 Lua worker 重启。

## 9. 回滚和重新接管

### 9.1 独立模式快速回滚

1. 立即将流量切回控制面节点或备用实例。
2. 在独立节点恢复上一个已验证的 `release-id`，先执行 `syedge -t`。
3. 仅在检查通过后 reload；保留失败版本和错误日志，便于定位。

### 9.2 从独立模式恢复控制面

1. 停止独立节点流量，保留独立配置和依赖备份。
2. 恢复控制面最后成功版本或重新发布已审计版本。
3. 确认本地不再有需要保留的手工差异后，重新启用并启动 Agent：

   ```bash
   sudo systemctl enable syedge-agent
   sudo systemctl start syedge-agent
   ```

4. 如果控制面要求重新注册，使用正式 enrollment 流程；不要删除 `/var/lib/syedge-agent` 后盲目重新生成身份。
5. 重新执行 `syedge -t`、控制面版本确认和业务冒烟。

### 9.3 卸载 Lua 扩展

只有在最终配置已经不再包含 `modules/lua/`、`*_by_lua`、`lua_package_*` 等内容后，才考虑执行扩展卸载。卸载脚本会拒绝仍被配置引用的 Lua 模块。通常只需要保留扩展安装，不要在本次模式切换中卸载它。

## 10. 日常运维和升级规则

- 每个发布版本保存配置、Lua 脚本、第三方库、扩展包和 `MANIFEST.sha256`。
- SyEdge 升级必须同步验证同版本 `syedge-lua`；GMSSL/Tongsuo 节点不能混用 standard 包。
- 升级前先在灰度节点执行 `require` 自检、语法检查、业务冒烟和回滚演练。
- 不要把备份后缀文件、`.lua-bak`、`.lua20*` 放进活动加载目录，避免误引用；备份应放在 release 目录之外或明确的归档目录。
- 继续使用 `syedge` 非 root 运行；不要在配置中添加 `user root` 来解决文件权限问题。
- Lua 不要同步写数据库作为请求成功的必要条件。需要审计/统计时，优先使用访问日志、Agent 或异步消息；确需 MySQL 时要限流、超时、连接池、凭据隔离和失败降级。
- 任何由请求头、URI、查询参数计算出的代理目标都必须经过固定 allowlist 校验。
- 第三方 C Lua 模块只允许来自经过签名和兼容性验证的构建产物。

## 11. 上线放行门槛

以下项目全部通过后才能扩大流量：

| 门槛 | 通过标准 |
|---|---|
| 控制面导入 | 导入报告无未处理的 include、端口、证书、静态目录和变量问题 |
| 版本匹配 | SyEdge、`syedge-lua`、LuaJIT、架构和 GMSSL/standard 变体一致 |
| 配置检查 | `syedge -t` 和 `syedge -T` 通过，日志无模块/变量缺失 |
| Lua 依赖 | 所有活动脚本的 `require`、`dofile`、资源路径都能在服务身份下加载 |
| 安全性 | 动态代理目标有 allowlist；无客户端网关绕过、任意文件加载和 SQL 拼接路径 |
| 功能验证 | kkfile、路由、ignore-gateway、HTTPS、WebSocket、静态文件和异常路径通过 |
| 可回滚性 | 已保存控制面版本、独立版本、依赖包和可执行回滚命令 |
| 接管边界 | Agent 已停止/禁用，或已使用蓝绿独立实例；没有两个配置源同时写入 |

## 12. 相关源码和文档

- [Lua 运行模式说明](lua.md)
- [安装、维护和独立配置说明](install-maintenance.md)
- [当前主配置](../demo/nginx.conf)
- [当前后台配置](../demo/houtai.conf)
- [当前前台配置](../demo/qiantai.conf)
- [当前 Lua 目录](../demo/lua/)

