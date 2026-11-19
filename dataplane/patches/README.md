# 改名 patch

只允许存在一个 patch：`0001-syedge-rename.patch`，基于 `dataplane/UPSTREAM` 锁定的上游版本（当前 1.30.6）。

**规则**：只替换字符串字面量与宏的值；禁止改动判断、结构体、函数签名、模块 ABI。
字符串长度变化时，与之配套的长度常量（例如 `ngx_cpymem(errstr, "SyEdge: ", 8)` 中的 8、HPACK 数组的长度）随之改变，这是字面量改名的直接后果，不算改动判断；
补丁由 `dataplane/gen-rename-patch` 生成并与已提交的补丁逐字节对比（`TestDefaultBrandMatchesCommittedPatch`），不手工编辑。
升级上游 = 在新版本上重新应用 patch，`build-syedge.sh` 会自动运行 `scripts/rename-scan.sh`；出现冲突或扫描失败即停，人工评审。

## 覆盖位置（1.30.6，共 20 个文件）

| 文件 | 改动 | 用户可见影响 |
| --- | --- | --- |
| src/core/nginx.h | `NGINX_VER` 前缀 `syedge/`；`NGINX_VAR` 改为 `SYEDGE` | `-v`/`-V`、Server 完整版本、热升级环境变量 |
| src/core/nginx.c | `SyEdge version:`、`Usage: syedge` | `-v` / `-h` |
| src/core/ngx_log.c | stderr 前缀 `syedge: `；“built without debug” | `syedge -t` 输出、启动错误 |
| src/core/ngx_syslog.c | syslog 默认标签 `syedge` | 系统日志 |
| src/core/ngx_regex.c | “built without PCRE JIT” | error.log |
| src/os/unix/ngx_setproctitle.c | 进程标题前缀 `syedge: ` | `ps aux` |
| src/http/ngx_http_header_filter_module.c | `Server: SyEdge` | HTTP/1.x 响应头 |
| src/http/ngx_http_special_response.c | `<center>syedge</center>` | 默认错误页 |
| src/http/v2/ngx_http_v2_filter_module.c | HPACK 编码 `{0x85,0xdd,0xeb,0x04,0x93,0x17}`（字符串 `SyEdge`，由 `gen-rename-patch` 用 `golang.org/x/net/http2/hpack` 生成）及调试日志 | HTTP/2 server 头 |
| src/http/v3/ngx_http_v3_filter_module.c | server 值（3 处） | HTTP/3 server 头 |
| src/http/ngx_http_variables.c、src/stream/ngx_stream_variables.c | 变量 `$syedge_version` | 配置变量（不兼容点，导入存量配置时自动替换） |
| src/event/ngx_event_openssl.c | OpenSSL appname；“built with Session Tickets” | openssl.cnf 段名、error.log |
| src/event/ngx_event.c | 共享内存名；“using --with-debug” | — |
| src/event/quic/ngx_event_quic_bpf.c | 环境变量 `SYEDGE_BPF_MAPS`；日志文案 | 热升级环境变量 |
| src/http/ngx_http.c、modules/ngx_http_ssl_module.c、modules/ngx_http_log_module.c、modules/ngx_http_image_filter_module.c、src/stream/ngx_stream_log_module.c | “syedge was built ...” | error.log |

C 标识符（如 `ngx_http_variable_nginx_version`、HTTP/2 中的数组名）保持原样，发布构建会剥离符号，二进制中不残留。
