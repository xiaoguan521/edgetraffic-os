# 从 OpenResty 迁移到 SyEdge

本文列出从 OpenResty 迁移时需要核对的差异：哪些能直接沿用，哪些指令和模块不可用，Lua 的边界，基线默认值的不同，以及两条迁移路线。
结论以本仓库的构建脚本、基线模板和现有文档为准；具体版本的编译参数以 `syedge -V` 输出为准。

## 1. 能直接沿用的

- **配置语法**：`http`、`server`、`location`、`upstream`、`proxy_pass`、`rewrite`、`return`、`map`、`geo`、`limit_req`、`limit_conn`、大部分 `ssl_*` 指令的语法与 Nginx 相同（见 [安装维护手册](install-maintenance.md) 8.5 节）。
- **命令行**：`-t`（校验配置）、`-T`（输出展开后的完整配置）、`-s reload` / `-s reopen` / `-s quit`、`-v` / `-V`、`-c`（指定配置文件）、`-p`（指定安装根目录）。对照表见 [快速上手](quickstart.md) 第 4 节，完整说明见 [安装维护手册](install-maintenance.md) 8.6 节。
- **信号**：reload 为 HUP，quit 为 QUIT，stop 为 TERM，reopen 为 USR1，与 Nginx 系列一致。日常操作请使用 `systemctl reload syedge` 或 `syedge -s reload`。
- **已启用的模块**（来自 `dataplane/build-syedge.sh`）：
  - `ssl`、HTTP/2、HTTP/3、`realip`、`stub_status`、`gzip_static`、`sub`、`auth_request`；
  - `stream`、`stream_ssl`、`stream_ssl_preread`；
  - 线程池与 file aio；
  - 上游默认内置的 `gzip`、`proxy`、`rewrite`、`limit_req`、`limit_conn`、`map`、`geo` 等。

## 2. 指令与模块差异

本表只列 OpenResty 常见而 SyEdge 缺少的模块与指令。SyEdge 没有任何第三方 C 模块；`http_auth_basic` 在编译时被关闭。

| OpenResty 中的写法 | SyEdge 是否支持 | 替代做法 |
| --- | --- | --- |
| `more_set_headers` | 否（无 headers-more 模块） | server 级 `add_header`。不要在 location 内写 `add_header`：location 内只要出现一条，server 级的全部 `add_header` 在该 location 都不再生效。启用 Lua 后也可在 `header_filter_by_lua_block` 中用 `ngx.header` 设置。 |
| `more_clear_headers` | 否 | 清除上游响应头用 `proxy_hide_header`（上游内置指令；基线已对 `X-Powered-By` 生效）。 |
| `auth_basic` / `auth_basic_user_file` | 否（编译时关闭 `http_auth_basic`） | 控制台托管：使用“外部鉴权”规则，对节点组内的上游发子请求，2xx 放行，401 / 403 原样返回（见 [Lua 与原生规则](lua.md)）。独立模式：`auth_request` 模块已编译，可自备返回 2xx / 401 / 403 的鉴权服务；或在 `access_by_lua_block` 中实现（需 Lua 附加包）。SyEdge 不读取 htpasswd 文件，无法直接沿用用户名密码文件。 |
| `echo` / `echo_exec` 等 | 否 | `return 200 "文本";`；或 Lua 的 `content` 阶段（`ngx.say`）。 |
| `set_escape_uri` 等 set-misc 指令 | 否 | 启用 Lua 后，转义类可用 `set_by_lua_block` 调用 `ngx.escape_uri` / `ngx.unescape_uri` 代替；其余指令无，需评估。 |
| `redis2_*` | 否 | 无，需评估 |
| `memc_*` | 否 | 无，需评估 |
| `srcache_*` | 否 | 无，需评估 |
| `drizzle_*` | 否 | 无，需评估 |
| `rds_json` | 否 | 无，需评估 |
| `encrypted_session` | 否 | 无，需评估 |
| `slice` | 否（未编译 `http_slice`） | 无，需评估 |
| `mp4` | 否（未编译 `http_mp4`） | 无，需评估 |
| `dav` | 否（未编译 `http_dav`） | 无，需评估 |
| `secure_link` | 否（未编译 `http_secure_link`） | 无，需评估 |
| `addition` | 否（未编译 `http_addition`） | 无，需评估 |
| `gunzip` | 否（未编译 `http_gunzip`） | 无，需评估。注意 `gzip_static` 已启用，它只发送预先压缩好的 `.gz` 文件，不做解压。 |
| `random_index` | 否（未编译 `http_random_index`） | 无，需评估 |
| `stream_realip` | 否（未编译 `stream_realip`） | 无，需评估 |
| `mail` | 否（未编译） | 无，需评估 |

## 3. Lua 差异

Lua 附加包 `syedge-lua` 与 `syedge` 版本绑定，标准版与国密版各一套，安装方法见 [安装维护手册](install-maintenance.md) 7.1 节。

**附加包中有：**

- LuaJIT 2.1（静态链入 Lua 模块）；
- `ndk_http_module`、`ngx_http_lua_module`；
- `lua-resty-core`、`lua-resty-lrucache`；
- 审核过的规则库 `etos.init`、`etos.json`、`etos.jwt`。

**附加包中没有：**

- `cjson`（JSON 请用 `etos.json`）；
- `resty.redis`、`resty.mysql`、`resty.http`、`resty.dns`、`resty.lock`、`resty.limit.*`、`resty.upstream.healthcheck`、`resty.websocket`、`resty.string`；
- stream 的 Lua 模块；
- `resty` 命令行与 `opm`。

**两种使用方式的区别：**

- **独立模式**：受信任的运维人员可以在手工维护的 `conf.d/*.conf` 中直接使用原生 Lua 指令（如 `access_by_lua_block`）。可用的指令与验收夹具见 [docs/lua.md](lua.md) 的“独立模式手工 Lua”一节。注意：基线主配置默认不加载 Lua 模块，装完附加包后还需要在主配置最前面加载 `R/modules/lua/` 下的 `ndk_http_module.so` 与 `ngx_http_lua_module.so`（先 ndk 后 lua），并在 `http` 内把 `lua_package_path` 指向 `R/modules/lua/lualib/?.lua`，否则 `*_by_lua*` 指令会被报为未知指令。`R` 是安装根目录，见安装维护手册的[路径约定](install-maintenance.md#路径约定)。
- **控制台托管**：Lua 只能以“自定义 Lua 脚本”片段受控开放，是受限环境。关键限制：
  - 只有管理员可以提交，并须由另一位管理员批准，需要 `lua` 授权，节点组内每台节点都要装附加包；
  - 只支持 `rewrite`、`access`、`content`、`header_filter`、`body_filter` 五个阶段，每段最大 16 KiB；
  - 不能使用 `io`、`os.execute`、`load`、`dofile`、FFI、`debug`，`require` 只允许 `etos.json`，不能使用 `ngx.socket`；
  - 不能写全局变量，请求之间不共享状态。

  完整说明见 [docs/lua.md](lua.md) 第 5 节。

若迁移时需要原样保留现网的原生 Lua 与第三方库，可把节点组的“配置接管方式”设为“兼容配置”（`legacy`），由该节点继续使用本地配置。说明见 [现有配置的观察接入与 Lua 兼容](legacy-config-management.md)。

## 4. 基线默认值差异

SyEdge 的基线配置比上游 Nginx 默认值更严格。下表来自 `internal/render/templates/main.conf.tmpl`。

| 项 | SyEdge 基线 | 上游默认值 |
| --- | --- | --- |
| 未匹配域名的请求 | HTTP 返回 444（断开），HTTPS 拒绝握手 | 落到第一个 server |
| `client_max_body_size` | `1m` | `1m`（相同；OpenResty 用户常调大，迁移时需按站点覆盖） |
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

此外，基线还设置了 `proxy_hide_header X-Powered-By;`，上表未列出。

**覆盖方法（独立模式）**：在自己的 `conf.d/*.conf` 的 server 块中覆盖即可，覆盖写在 `conf.d` 中，不要改动基线主配置。例如在已有 server 块内：

```nginx
server {
    server_name  www.example.com;
    # ... 原有的 listen、ssl、proxy 配置 ...

    client_max_body_size  50m;              # 站点级上传上限

    location /api/upload/ {
        client_max_body_size  200m;         # 只对上传接口放宽
        proxy_pass            http://127.0.0.1:8080;
    }

    location /api/report/ {
        proxy_read_timeout  300s;           # 长请求
        proxy_pass          http://127.0.0.1:8080;
    }
}
```

注意：location 内一旦写 `proxy_set_header`，http 层的 5 条基线 `proxy_set_header` 会全部失效，需要在该 location 中重新写全：

```nginx
location /api/ {
    proxy_set_header  Host               $host;
    proxy_set_header  X-Real-IP          $remote_addr;
    proxy_set_header  X-Forwarded-For    $proxy_add_x_forwarded_for;
    proxy_set_header  X-Forwarded-Proto  $scheme;
    proxy_set_header  X-Request-ID       $request_id;
    proxy_pass        http://127.0.0.1:8080;
}
```

## 5. 迁移步骤

**路线 A：独立模式，逐站点迁到 `conf.d`**

1. 按 [快速上手](quickstart.md) 第 2 节安装 `syedge`，并用其中的校验命令确认基线配置通过。下文的 `R` 是安装根目录（`R=$(sudo syedge-instance prefix)`）。
2. 把每个站点的 `server`、`upstream`、`location` 从旧配置复制到 `$R/conf/current/conf.d/<站点>.conf`，不要用旧的 `/etc/nginx` 或 `openresty` 目录整体覆盖 `conf/current`（见 [安装维护手册](install-maintenance.md) 8.2、8.5 节）。
3. 按第 2 节替换不支持的指令；需要 Lua 的站点先按 [安装维护手册](install-maintenance.md) 7.1 节安装附加包，并按第 3 节加载模块。
4. 对照第 4 节核对基线默认值，需要时在 server 或 location 内覆盖，然后再次校验。
5. 校验通过后执行 `sudo systemctl reload syedge`，先用测试域名验证，再切换业务流量。

**路线 B：控制台托管，“配置导入”加观察接入**

1. 在控制台新建节点组，先把“配置接管方式”设为“观察接入”或“兼容配置”，再注册节点（见 [现有配置的观察接入与 Lua 兼容](legacy-config-management.md) 的推荐流程）。
2. 节点已有独立配置时，注册时加 `-replace-local-config`，原配置会备份到 Agent 的备份目录（见 [快速上手](quickstart.md) 第 3 节）。
3. 在“配置导入”中上传配置。OpenResty 的 `openresty -T` 输出格式在本仓库中未验证，请以导入报告为准；可改用配置目录的 `tar.gz`（见 [安装维护手册](install-maintenance.md) 8.5 节）。
4. 核对导入报告，处理不支持的指令、缺失的 `include` 与 Lua 脚本（按第 2、3 节处理），确认后“应用为草稿”。
5. 把节点组切回“控制台托管”，人工预览、审批并显式发布。切回不会自动发布或覆盖现网。

## 6. 迁移前自查清单

先设置旧配置目录，再运行下面的命令。命中的行按第 2 节处理。

```bash
OLD_CONF=/usr/local/openresty/nginx/conf
grep -rnE 'more_set_headers|more_clear_headers|auth_basic|echo |set_escape_uri|redis2_|memc_|srcache_|drizzle_|rds_json|encrypted_session' "$OLD_CONF"
```

```bash
OLD_CONF=/usr/local/openresty/nginx/conf
grep -rnE '^\s*(slice|mp4|dav_[a-z_]+|secure_link[a-z_]*|addition_[a-z_]+|gunzip|random_index|stream_realip)\b' "$OLD_CONF"
```

Lua 依赖（`require` 的 `resty.*` 模块与 `cjson`），以及 Lua 指令的使用位置：

```bash
OLD_CONF=/usr/local/openresty/nginx/conf
grep -rnE "require[^)]*resty\.|cjson|lua_package_path|_by_lua" "$OLD_CONF"
```

自查后请确认：

- 所有命中的指令都已按第 2 节替换或确认无需替换；
- Lua 依赖均在第 3 节的“附加包中有”清单内，否则需要改写或保留在兼容模式；
- 需要放宽的上传接口、长请求接口已在 `conf.d` 中覆盖（第 4 节）；
- 使用 `proxy_set_header` 的 location 已重写 5 条基线头。
