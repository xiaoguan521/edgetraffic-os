# 兼容配置接入 SkyWalking：Lua 库怎么放

本示例面向原生兼容配置，安装根目录 `R`（默认 `/opt/syedge`，以 `syedge-instance prefix` 为准）。

## 1. 三步接入（运维只改三个值）

示例目录里已经带好了现成的库 `lualib/`，不用再解压压缩包、挑文件。

```bash
R=/opt/syedge                                  # 安装根目录，以 syedge-instance prefix 为准
sudo cp -r lualib/. "$R/lualib/"               # 1. 整个目录原样复制，不要改里面的文件
sudo cp conf.d/10-skywalking.conf "$R/conf/current/conf.d/"   # 2. 初始化文件
sudo vi "$R/conf/current/conf.d/10-skywalking.conf"           # 3. 只改开头三个值
```

第 3 步只改 `init_worker_by_lua_block` 开头这三行：

```lua
local OAP_ADDRESS   = "http://192.0.2.10:22800"   -- SkyWalking OAP 的 HTTP 上报地址
local SERVICE_NAME  = "example-service"           -- 服务名
local INSTANCE_NAME = "example-node-1"            -- 实例名，多个节点同时上报时必须各不相同
```

OAP 地址如果还是示例值（192.0.2.10），worker 启动时会在 `error.log` 里写“请先修改 OAP_ADDRESS”并且不上报，不会去连一个不存在的地址刷错误。

`lualib/` 放在 **`R/lualib/`**（你自己的库目录，附加包升级不会覆盖）。不要放进 `R/modules/lua-<版本>/`，那是附加包自带目录，升级会被替换。
`resty/core.lua` 不用放，附加包里已有。库的来源、许可见 `lualib/README.txt`。

指标名沿用 OpenResty 时代的 `nginx_http_*`（含 `nginx_http_request_duration_seconds`），已有的 Grafana 面板不用改。

## 2. 配置文件放哪

```
R/conf/current/
├── syedge-443.conf          主配置（本目录的示例 syedge-443.conf）
└── conf.d/
    └── 10-skywalking.conf   共享内存 + init_worker_by_lua_block
```

主配置里已有 `include conf.d/*.conf;`，所以只需把 `10-skywalking.conf` 放进 `conf.d/`。
放之前确认 `conf.d/` 下没有别的文件也定义了 `init_worker_by_lua_block`、`lua_shared_dict tracing_buffer` 或 `lua_package_path`，同级别重复会让 `-t` 报错。

## 3. 主配置的接入要点

示例 `syedge-443.conf` 体现了这几条，改自己的配置时对照：

1. `lua_package_path` 的**第一项**是 `R/lualib`，让 `require("skywalking.xxx")` 能找到库；附加包自带目录放在后面。
2. 带链路的 location 里 `rewrite_by_lua_block`、`body_filter_by_lua_block`、`log_by_lua_block` 三段必须齐全。
3. 不要写 server 级的 `log_by_lua_block`：它会被所有没有自己 `log_by_lua` 的 location 继承，在没有执行过 `start()` 的请求上调用 `prepareForReport()`。
4. `:9145/metrics` 依赖的 `prometheus`、`metric_connections` 由 `10-skywalking.conf` 创建，主配置里不要重复定义，并用 `allow/deny` 限制来源。

这些要点由 `scripts/ci/skywalking-conf-check.sh` 静态校验。

## 4. 校验与生效

```bash
R=/opt/syedge
$R/sbin/syedge -p $R/ -c $R/conf/current/syedge-443.conf -e $R/logs/error.log -t
kill -HUP $(cat $R/logs/syedge-443.pid)     # reload，不要 restart
```

检查：

```bash
curl -s http://127.0.0.1:9145/metrics | head
tail -n 50 $R/logs/error.log
```

- `error.log` 不应再出现 `attempt to index global 'skywalking_tracer'`；
- 如果出现 `connection refused`，说明 `OAP_ADDRESS` 填的地址没人监听（常见原因是还保留着示例值），不影响业务，但链路不上报；
- 回退：恢复备份的 `conf/current`，删除 `conf.d/10-skywalking.conf`，再 `-t` 并 HUP。

## 5. 已知限制

- location 里的 `log_by_lua_block` 会覆盖 http 级别的同阶段指令，所以带链路的 location 不会统计请求数与延迟，只有未带链路的 location 有数据。
- 这是原生兼容配置，改动要写进提交的配置包，直接改节点文件会被下次发布覆盖。
