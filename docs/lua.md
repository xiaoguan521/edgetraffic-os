# 访问规则与 Lua 扩展（P10）

规格书 12.5。分两层：

| 能力 | 依赖 | 授权 |
| --- | --- | --- |
| 原生规则积木：IP / IP 集（地域）/ 请求头 / User-Agent / 路径 / 方法 → 拒绝 / 跳转 / 限速 / 外部鉴权 | 只用官方模块，标准版与国密版基础包都能用 | `core`（所有版本） |
| Lua 规则：JWT 校验（HS256 / RS256） | 节点安装 `syapp-lua` 附加包 | `lua`（专业版、企业版） |

不开放客户自定义 Lua 脚本：
- Lua 代码只由 `internal/render` 的固定模板生成；
- 参数写成 `\ddd` 转义的字面量；
- 密钥只写进版本目录的 `lua/*.key` 文件（0640，预览中不显示）。

## 原生规则积木

每条规则由 1–8 个条件（同时满足）和 1 个动作组成，每个站点最多 32 条。

执行顺序由请求处理阶段决定：
1. 拒绝 / 跳转（rewrite）；
2. 限速（preaccess）；
3. 外部鉴权（access）；
4. JWT（access，在外部鉴权之后）。

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

## syapp-lua 附加包

一个包同时适配标准版与国密版基础包，内容固定为：
- LuaJIT 2.1（OpenResty 分支）；
- ngx_devel_kit、OpenResty 的 lua 模块（动态模块）；
- lua-resty-core、lua-resty-lrucache；
- 审核过的规则库 `etos`（JSON 解析、JWT 校验、初始化与沙箱）。

版本与提交哈希锁定在 `dataplane/lua/DEPS`。syapp 核心与改名 patch 不变。

基础包为此做了两处调整，都不改变不装附加包时的行为：
- 编译外部鉴权需要的 `auth_request` 模块；
- 固定导出附加包需要的 OpenSSL / 铜锁函数（`dataplane/lua/base-exports.txt`）。

### 安装与卸载（节点上以 root 执行）

```
mkdir syapp-lua && tar -C syapp-lua -xzf syapp-lua-1.30.6-x86_64.tar.gz && cd syapp-lua
bash install.sh      # 校验与运行中的 syapp 版本一致，自动选择标准版 / 国密版模块，真实 syapp -t 自检
bash uninstall.sh    # 当前配置仍使用 JWT 规则时拒绝；先在控制台去掉 JWT 规则并发布
```

安装的具体行为：
- 模块装到 `/opt/syapp/modules/lua-<版本>/`，配置通过软链 `/opt/syapp/modules/lua` 加载；
- 安装、卸载都不重启、不停止 syapp；
- agent 在心跳中上报附加包，节点列表显示“Lua”标签。

### 发布门禁与回退

满足下面两点，控制台才允许发布含 JWT 规则的版本：
1. 授权包含 `lua`；
2. 节点组内每个节点都上报了与其基础变体匹配的附加包。

否则预览与发布报 `LUA_ADDON_REQUIRED`，并列出缺少附加包的节点。

万一含 Lua 的版本到达未装附加包的节点，`syapp -t` 会因找不到模块而失败，agent 自动回退到原版本。

### 数据面热升级

模块与 syapp 二进制版本强绑定，所以装有附加包的节点升级时要带上同版本的附加包：
1. 厂商离线签名两个包：

   ```
   syapp-license-signer sign-package ... -name syapp     -version <版本> -arch amd64 -variant standard|gmssl
   syapp-license-signer sign-package ... -name syapp-lua -version <版本> -arch amd64 -variant lua
   ```

   附加包的 `addon/VERSION` 中的版本必须与 `<版本>` 一致（`build-syapp-lua.sh -v <版本>`）。
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

## 验收（CI：`.github/workflows/lua.yml`）

amd64 / arm64 × 标准版 / 国密版共 4 组，在干净的机器上以生产路径运行 `scripts/ci/lua-acceptance.sh`，覆盖：
- 模块加载与真实 `syapp -t`（含渲染器黄金用例）；
- 原生规则、外部鉴权、限速；
- JWT HS256 / RS256，以及各种非法令牌；
- 沙箱；
- reload、带附加包的热升级与中止；
- 标准 HTTPS 与国密 NTLS；
- 安装、拒绝卸载、卸载；
- 品牌检查：模块目录 `/opt/syapp/modules/lua-*/` 之外，二进制、配置、日志中没有上游名；
- 性能：开启三条常用规则（IP 拒绝、UA 拒绝、JWT 校验）后 QPS 下降不超过 15%（交替三轮压测取中位数）。
