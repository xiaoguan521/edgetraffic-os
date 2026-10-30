# 控制台主备高可用（P9）

需要授权功能 `ha`（专业版、企业版）。

## 原理

- 两台（或多台）控制台共用同一个 PostgreSQL，用会话级 advisory lock 选主。
- 只有主控制台接受节点连接、提供 Web 控制台与 API、运行后台任务（证书续期、告警评估、数据清理）。
- 备控制台对 `/healthz` 返回 503（`"role":"standby"`），其余 API 返回 503 `STANDBY`，节点的 gRPC 连接返回 `Unavailable`。
- 主控制台进程退出、主机宕机或与数据库断开时，数据库会话结束，锁随之释放；备控制台在 3 秒内接管。
- 主控制台每 3 秒在持锁连接上探活，探活失败会立即降级：停止后台任务，断开所有节点。
- 节点连接 VIP，断线后自动重连到新的主控制台；节点上已生效的配置不受影响（数据面不停、不重启）。
- 只要使用 PostgreSQL，控制台就总是参与选主。因此误启动的第二台控制台不会与主控制台同时下发。
- 没有 `ha` 授权时，备控制台不会自动接管。

## 部署

1. 准备 PostgreSQL（建议自身做主从或使用托管服务），两台控制台的 `-db` 指向同一个 DSN。
2. 先启动第一台控制台，完成初始化并导入 License。高可用 License 请使用 `installation_only` 绑定：安装 ID 存在共用数据库中，主备相同；主机指纹各不相同。
3. 从第一台的数据目录（默认 `/var/lib/syapp-console`）把 `pki/` 和 `secret.key` 复制到第二台的数据目录（权限 0700 / 0600）：
   - `pki/` 里是节点 CA 和 Web CA。节点只信任这套 CA，主备不同，切换后节点将无法连接。
   - `secret.key` 是库中证书私钥、告警凭据等的加密密钥。
   - 控制台启动时会校验两者的指纹与库中一致；不一致会拒绝启动，并提示复制哪个文件。
4. `-grpc-hosts` / `-web-hosts` 包含 VIP，`-grpc-advertise` 填 `VIP:9443`。这样证书里有 VIP，节点安装命令也指向 VIP。
5. 两台都以相同参数启动第二台控制台，日志出现 `ha=true`。后启动的一台是备。
6. 用 keepalived 管理 VIP：按 `/healthz` 结果漂移，VIP 始终在主控制台上。

## keepalived 示例

两台控制台安装 keepalived，`/etc/keepalived/keepalived.conf`：

```
vrrp_script chk_console {
    # 主控制台返回 200，备控制台返回 503
    script "/usr/bin/curl -sfk -o /dev/null --max-time 2 https://127.0.0.1:8443/healthz"
    interval 2
    fall 2
    rise 1
    weight -50
}

vrrp_instance ETOS_CONSOLE {
    state BACKUP            # 两台都写 BACKUP，由健康检查决定谁持有 VIP
    nopreempt
    interface eth0
    virtual_router_id 51
    priority 100            # 第二台写 90
    advert_int 1
    authentication {
        auth_type PASS
        auth_pass change-me
    }
    virtual_ipaddress {
        10.0.0.100/24
    }
    track_script {
        chk_console
    }
}
```

- 若控制台监听 `127.0.0.1` 以外的地址，把检查地址改成实际地址。
- 若未使用 `-tls-auto` 或 `-tls-cert`，改用 `http://`。
- 使用云负载均衡时，把健康检查路径设为 `/healthz`、期望状态码设为 200 即可，不需要 keepalived。

## 验证

```
curl -sk https://<控制台A>:8443/healthz   # {"role":"leader","db":"ok",...}
curl -sk https://<控制台B>:8443/healthz   # {"role":"standby",...}，HTTP 503
systemctl stop syapp-console               # 在 A 上执行
curl -sk https://<控制台B>:8443/healthz   # 数秒内变为 leader
```

自动化验证见 `scripts/ci/console-smoke.sh` 的“主备高可用”一节（PostgreSQL 运行时执行）。
