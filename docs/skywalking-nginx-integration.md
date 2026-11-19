# 独立版接入 SkyWalking 链路与 Prometheus 监控

适用：节点以**独立模式**运行（未注册到控制台），用 `skywalking-nginx-lua-0.5.0` 做链路上报，
用其中的 `nginx-lua-prometheus` 暴露 `/metrics`，再由 OpenTelemetry Collector 抓取。

> 本文按静态核对写成，未在真实节点上跑过完整链路；第 9 节给出验证步骤，首次上线请先在单节点演练。
> 控制台托管（managed）节点不适用：托管 Lua 禁用 `ngx.socket`、`require` 外部库，本方案会失败。

## 1. 与原 OpenResty 方案的对应关系

| 原方案 | 我们的做法 |
| --- | --- |
| 源码编译 OpenResty ≥ 1.15.8.3 | 不编译。安装 syedge 基础包 + 与之**同版本、同架构、同加密变体**的 `syedge-lua` 附加包 |
| `configure` 加模块（iconv、vts 等） | 不需要；本方案只用到 lua 模块、`stub_status`（基础包已内置），无需 vts |
| 安装目录 `/apps/svr/openresty` | 安装根目录 `R`（默认 `/opt/syedge`，以 `sudo syedge-instance prefix` 为准） |
| 主配置 `nginx.conf` 里 `include conf.d/*.conf` | 基线主配置已在 http 内带 `include conf.d/*.conf;`，不用改主配置 |
| `lua_package_path "/home/skywalking-nginx-lua-0.5.0/lib/?.lua;;"` | **不要再写**（见第 3 节），把 Lua 文件放进 `R/lualib/` |
| `openresty -t` / 启动 | `syedge -t` / `systemctl reload syedge`，**不得 restart** |

## 2. 安装 Lua 附加包并启用独立模式

附加包的下载与校验见 [安装维护手册](install-maintenance.md)。解包后以 root 执行：

```bash
cd /tmp/syedge-lua
sudo bash install.sh --standalone     # 安装附加包，加载 lua 模块，生成 conf.d/00-lua-standalone.conf（不 reload）
```

前置条件（任一不满足即报错退出，不改动文件）：节点未注册控制台、`conf/current` 指向 `r000000`、主配置没有手工 `load_module`。
该命令会写入 `lua_package_path`（`lualib`、`extlib`、`R/lualib` 三段）和 `lua_package_cpath`，并带上 `cjson` 等编译库。
`resty.core`、`resty.lrucache`、`tablepool`、`cjson` 附加包都已随附，不用再装。

## 3. 放置 skywalking 与 prometheus 的 Lua 文件

不要新写 `lua_package_path`：`00-lua-standalone.conf` 已经设置，同一 http 级别重复设置会让 `syedge -t` 报 duplicate。
解压 `skywalking-nginx-lua-0.5.0.zip`，把 `lib/` 下的内容复制到 `R/lualib/`：

```bash
R=$(sudo syedge-instance prefix)
unzip skywalking-nginx-lua-0.5.0.zip -d /tmp/sw
SRC=/tmp/sw/skywalking-nginx-lua-0.5.0/lib

sudo mkdir -p "$R/lualib/resty"
sudo cp -r  "$SRC/skywalking"                  "$R/lualib/"
sudo cp     "$SRC"/prometheus*.lua             "$R/lualib/"   # 不需要 prometheus_test.lua，可删
sudo cp     "$SRC/resty/http.lua" "$SRC/resty/http_headers.lua" "$SRC/resty/jit-uuid.lua" "$R/lualib/resty/"
sudo rm -f  "$R/lualib/prometheus_test.lua" "$R/lualib"/skywalking/*_test.lua
sudo chown -R root:syedge "$R/lualib" && sudo chmod -R o-w "$R/lualib"
```

说明：
- `resty.http` 不在附加包内（附加包不随附 lua-resty-http），必须用 skywalking 包自带的这份。
- 如果 `R/lualib/resty/` 下已有同名文件，先确认不是你自己的版本再覆盖。
- 具名实例用 `--instance <名>`，其 `R` 为 `R/instances/<名>`，见 [docs/lua.md](lua.md)。

## 4. 新增 `conf.d/10-skywalking.conf`（http 级别）

写到 `R/conf/current/conf.d/10-skywalking.conf`。把 OAP 地址改成接收上报数据的 HTTP 地址，服务名与实例名按现场命名：

```nginx
# Buffer 保存注册信息与待上报的 segment 队列
lua_shared_dict tracing_buffer 100m;
lua_shared_dict prometheus_metrics 10M;

init_worker_by_lua_block {
    local metadata_buffer = ngx.shared.tracing_buffer
    metadata_buffer:set('serviceName', 'web-nginx')
    metadata_buffer:set('serviceInstanceName', 'nginx_node1')
    metadata_buffer:set('includeHostInEntrySpan', false)

    require("skywalking.util").set_randomseed()
    -- 替换为 SkyWalking OAP 地址与 HTTP 上报端口
    require("skywalking.client"):startBackendTimer("http://127.0.0.1:22800")

    skywalking_tracer = require("skywalking.tracer")

    prometheus = require("prometheus").init("prometheus_metrics")
    metric_bytes = prometheus:counter(
        "nginx_http_size_bytes", "Total size of HTTP", {"type", "route"})
    metric_requests = prometheus:counter(
        "nginx_http_requests_total", "Number of HTTP requests", {"status", "route"})
    metric_latency = prometheus:histogram(
        "nginx_http_latency", "HTTP request latency", {"route"})
    metric_connections = prometheus:gauge(
        "nginx_http_connections", "Number of HTTP connections", {"state"})
}

log_by_lua_block {
    metric_bytes:inc(tonumber(ngx.var.request_length), {"request", ngx.var.host})
    metric_bytes:inc(tonumber(ngx.var.bytes_sent), {"response", ngx.var.host})
    metric_requests:inc(1, {ngx.var.status, ngx.var.host})
    metric_latency:observe(tonumber(ngx.var.request_time), {ngx.var.host})
}
```

与原文的差异：原文 `ngx.var.bytes_send` 是笔误，应为 `bytes_sent`。

## 5. 业务站点接入链路（server / location 级别）

在需要追踪的 location 里加（`proxy_pass` 的地址按实际修改）：

```nginx
location /test {
    rewrite_by_lua_block {
        skywalking_tracer:start("upstream service")
    }
    proxy_pass http://IP:9001;
    body_filter_by_lua_block {
        if ngx.arg[2] then
            skywalking_tracer:finish()
        end
    }
    log_by_lua_block {
        skywalking_tracer:prepareForReport()
    }
}
```

注意（基线规则）：location 内若出现 `proxy_set_header`，必须重发基线 5 条请求头；location 内不要 `add_header`。
一个 location 里的 `log_by_lua_block` 会覆盖 http 级别的同阶段指令，所以第 4 节的指标 `log_by_lua_block` 在该 location 内不再执行。
需要同时统计指标时，把指标代码并进这个 location 的 `log_by_lua_block`，或把第 4 节的指标代码改写成一个 Lua 模块函数，两处都调用。

## 6. 监控端口 `/metrics`

新增 `conf.d/20-metrics.conf`：

```nginx
server {
    listen 9145;
    allow 192.168.0.0/16;     # 只放行 OTel Collector 所在网段
    deny  all;

    location /metrics {
        content_by_lua_block {
            metric_connections:set(ngx.var.connections_reading, {"reading"})
            metric_connections:set(ngx.var.connections_waiting, {"waiting"})
            metric_connections:set(ngx.var.connections_writing, {"writing"})
            prometheus:collect()
        }
    }
}
```

`$connections_*` 变量来自 `stub_status` 模块，基础包已编译进去。
`/metrics` 不要对公网开放。节点防火墙放行 9145 仅限 Collector 地址。

## 7. 校验与生效

先校验（二进制路径以本机为准）：

```bash
R=$(sudo syedge-instance prefix)
sudo -u syedge "$R/sbin/syedge" -t -p "$R"
```

校验通过后 reload。**不要 restart syedge**：

```bash
sudo systemctl reload syedge
```

## 8. OpenTelemetry Collector 抓取

修改 `/etc/otelcol/otel-collector-config.yaml`，与原方案一致，仅 `targets` 指向节点 IP:9145：

```yaml
receivers:
  prometheus/11:
    config:
      scrape_configs:
        - job_name: 'nginx-monitoring'
          scrape_interval: 60s
          metrics_path: "/metrics"
          static_configs:
            - targets: ['节点IP:9145']
              labels:
                service: nginxName            # 自定义
                service_instance_id: nginxID  # 自定义
processors:
  batch/11:
exporters:
  otlp/11:
    endpoint: IP:21800
    tls:
      insecure: true
service:
  pipelines:
    metrics/11:
      receivers: [prometheus/11]
      processors: [batch/11]
      exporters: [otlp/11]
```

重启 Collector（这是 otelcol，不是 syedge）：`systemctl restart otelcol`。

## 9. 验证

1. `curl -s http://节点IP:9145/metrics | head`：出现 `nginx_http_connections`、`nginx_http_requests_total`。
2. 访问一次 `/test` 后再看 `/metrics`，请求计数增加。
3. 错误日志无 `skywalking` / `prometheus` 相关 `[error]`：`tail -f R/logs/error.log`。
4. SkyWalking 界面在约一分钟内能看到服务 `web-nginx`。
5. 节点需要能访问 OAP 的 HTTP 端口，不通时 `error.log` 会有 `resty.http` 连接失败。

## 10. 注意事项与限制

- 全部用 Lua 原生指令，依赖独立模式；节点一旦接入控制台，这些手工配置按独立模式接管规则处理（见 [legacy-config-management.md](legacy-config-management.md)），不会被渲染或预览。
- 停用：先删除上述 Lua 指令，再 `sudo bash install.sh --standalone-disable`；Lua 仍启用时 `uninstall.sh` 会拒绝卸载。
- `resty.http` 访问 https 的 OAP 需要 TLS 握手与证书校验配置；本方案按原文使用 http 端口。
- 附加包版本必须与 syedge 同版本、同架构、同加密变体（含国密/铜锁变体）。
