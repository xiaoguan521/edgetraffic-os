# 改名 patch

只允许存在一个 patch：`0001-syapp-rename.patch`，于 P0 基于锁定的上游版本生成。

**规则**：只替换字符串字面量与宏的值；禁止改动判断、结构体、函数签名、模块 ABI。
升级上游 = 在新版本上重新应用并运行 `scripts/rename-scan.sh`；出现冲突即停，人工评审。

必须覆盖的位置（规格书 4.2，按上游 2026-10-23 主分支核对）：

| 文件 | 改动 |
| --- | --- |
| src/core/nginx.h | NGINX_VER 前缀 `syapp/`；NGINX_VAR 改为 `SYAPP` |
| src/core/nginx.c | `-v` / `-h` 输出与帮助文本 |
| src/os/unix/ngx_setproctitle.c | 进程标题前缀 `syapp: ` |
| src/http/ngx_http_header_filter_module.c | `Server: syapp` |
| src/http/ngx_http_special_response.c | 默认错误页品牌字 |
| src/http/v2/ngx_http_v2_filter_module.c | HPACK 哈夫曼编码的 server 值（由 ../hpack-gen 生成） |
| src/http/v3/ngx_http_v3_filter_module.c | server 值（2 处） |
| src/os/unix/ngx_control.c | 环境变量 `SYAPP_CTRL` 与端点名（stable 中如无则跳过） |
| src/http/ngx_http_variables.c | 变量 `syapp_version` |
| src/event/ngx_event_openssl.c | OpenSSL appname |
| src/event/ngx_event.c | 共享内存名 |
| src/**/*.c | 错误日志中的产品名 |
