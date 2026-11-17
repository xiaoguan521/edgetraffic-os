# 兼容配置接入 SkyWalking：Lua 库怎么放

本示例面向原生兼容配置，安装根目录 `R`（默认 `/opt/syedge`，以 `syedge-instance prefix` 为准）。

## 1. 要放的文件

放到 **`R/lualib/`**（你自己的库目录，附加包升级不会覆盖它）。
不要放进 `R/modules/lua-<版本>/`，那是附加包自带目录，升级会被替换。

```
R/lualib/
├── skywalking/                 整个目录（去掉 *_test.lua）
├── prometheus.lua
├── prometheus_keys.lua
├── prometheus_resty_counter.lua
└── resty/
    ├── http.lua
    ├── http_headers.lua
    └── jit-uuid.lua
```

`resty/core.lua` 不用放，附加包里已有。测试文件与 `integration/` 不要放。

来源：解压 `demo/skywalking-nginx-lua-0.5.0.zip`（上游 skywalking-nginx-lua 0.5.0）得到的 `lib/`。

```bash
R=/opt/syedge
unzip skywalking-nginx-lua-0.5.0.zip -d /tmp/sw
SRC=/tmp/sw/skywalking-nginx-lua-0.5.0/lib

sudo mkdir -p "$R/lualib/resty"
sudo cp -r "$SRC/skywalking"      "$R/lualib/"
sudo cp    "$SRC"/prometheus*.lua "$R/lualib/"
sudo cp    "$SRC/resty/http.lua" "$SRC/resty/http_headers.lua" "$SRC/resty/jit-uuid.lua" "$R/lualib/resty/"
sudo rm -f "$R/lualib/prometheus_test.lua" "$R"/lualib/skywalking/*_test.lua
sudo chown -R root:syedge "$R/lualib" && sudo chmod -R o-w "$R/lualib"
```

## 2. 配置文件放哪

```
R/conf/current/
├── syedge-443.conf          主配置（本目录的示例 syedge-443.conf）
└── conf.d/
    └── 10-skywalking.conf   共享内存 + init_worker_by_lua_block
```

主配置里已有 `include conf.d/*.conf;`，所以只需把 `10-skywalking.conf` 放进 `conf.d/`。
放之前确认 `conf.d/` 下没有别的文件也定义了 `init_worker_by_lua_block`、`lua_shared_dict tracing_buffer` 或 `lua_package_path`，同级别重复会让 `-t` 报错。

`10-skywalking.conf` 里是示例值，按实际修改：服务名 `example-service`、实例名 `example-node-1`、OAP `http://192.0.2.10:22800`。多个节点同时上报时，`serviceInstanceName` 不能相同。

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
- 如果出现 `resty.http` 连接失败，说明连不上 OAP（`192.0.2.10:22800`），不影响业务，但链路不上报；
- 回退：恢复备份的 `conf/current`，删除 `conf.d/10-skywalking.conf`，再 `-t` 并 HUP。

## 5. 已知限制

- location 里的 `log_by_lua_block` 会覆盖 http 级别的同阶段指令，所以带链路的 location 不会统计请求数与延迟，只有未带链路的 location 有数据。
- 这是原生兼容配置，改动要写进提交的配置包，直接改节点文件会被下次发布覆盖。
