# SyEdge 安装与维护手册

本文按 Nginx 常用的安装、配置校验、平滑加载、日志维护方式编写，适用于 SyEdge 的控制台、边缘节点 `syedge`、节点 Agent，以及可选的国密和 Lua 能力。

第一次部署想先按步骤跑通，可以先看 [快速上手](quickstart.md)。

> 示例版本使用 `1.30.6`。实际操作时，把版本号、架构和文件名替换成发布包中的值。

## 1. 组件与安装方式

发布页每个文件的用途、交付对象与选择方法见 [`release-assets.md`](release-assets.md)（同一内容也写在每个正式版本的发布说明开头）。

| 组件 | 作用 | 默认位置 |
| --- | --- | --- |
| `syedge-console` | Web 控制台、REST API、节点 gRPC 网关 | `/usr/bin/syedge-console` |
| `syedge` | 边缘数据面，处理 HTTP/HTTPS/四层流量 | `/opt/syedge/sbin/syedge` |
| `syedge-agent` | 节点主动连接控制台、接收发布和采集状态 | `/usr/bin/syedge-agent` |
| `syedge-wrapper` | Agent 唯一的提权入口 | `/usr/libexec/syedge/syedge-wrapper` |
| `syedge-lua` | LuaJIT、Lua 动态模块和审核过的 Lua 库附加包 | `/opt/syedge/modules/lua-*` |

推荐选择：

1. 生产边缘节点使用 `syedge-node-<版本>-linux-<架构>.tar.gz` 或 RPM/DEB。
2. 控制台使用 `syedge-console` 发布包或 `syedge-console` RPM/DEB。
3. 只有在开发、移植或制作发行包时，才直接执行源码编译脚本。
4. 需要国密时，安装 `gmssl` 数据面包；标准版不能通过运行时配置切换为国密版。
5. 需要 Lua JWT 规则时，安装与 `syedge` 版本一致的 `syedge-lua` 附加包。

### 路径约定

本手册用 `R` 表示 system 模式的安装根目录，即程序、版本库、配置和数据目录所在的根。`R` 不一定是 `/opt/syedge`，以本机实际值为准。查询方法：

```bash
sudo syedge-instance prefix
```

该命令随 `syedge-agent` 软件包安装，直接输出 `R`。若 `/etc/syedge/prefix` 不存在，则按历史规则判断：已有 `/opt/syedge` 安装时返回 `/opt/syedge`（见 `layout.go` 中 `LoadSystemRootAt`）。

`R` 有两个常见的默认值，取决于程序是在哪里构建的：

- 本地构建未设置 `SYEDGE_PREFIX` 时，`R=/opt/syedge`（`Makefile` 中 `SYEDGE_PREFIX ?= /opt/syedge`）。
- GitHub Actions 发布的节点包默认 `R=/home/atwasoft/syedge`（`release.yml`、`packages.yml` 等工作流中的 `SYEDGE_PREFIX` 默认值）。

首次安装时可用 `sudo ./install.sh --prefix <目录>` 指定其他根目录，安装脚本会把它写入 `/etc/syedge/prefix`。

日志、缓存、agent 状态和 PID 目录与 `R` 的关系如下（代码依据：`internal/layout/layout.go`、`deploy/node/install.sh`）：

| 用途 | `R=/opt/syedge`（历史布局） | 其他根目录 `R` |
| --- | --- | --- |
| 日志 | `/var/log/syedge` | `R/logs` |
| 缓存 | `/var/cache/syedge` | `R/cache` |
| agent 状态 | `/var/lib/syedge-agent` | `R/agent` |
| PID 与运行目录 | `/run/syedge` | `/run/syedge`（不随 `R` 变化） |

## 2. 系统要求与端口

### 2.1 运行环境

- 边缘节点推荐 Linux + systemd，并需要 root 或 sudo 权限。
- 使用项目提供的静态依赖构建包时，数据面二进制要求 glibc 2.17 及以上；正式发布建议在 manylinux2014 容器中构建。
- 节点运行时不依赖系统 OpenSSL、PCRE2、zlib；国密包内置铜锁（Tongsuo）和 NTLS 支持。
- `syedge-agent` 只主动连接控制台，不监听管理端口。
- 控制台和节点的 CPU 架构必须与发布包匹配：常见值为 `amd64`/`x86_64` 和 `arm64`/`aarch64`。

### 2.2 网络端口

| 端口 | 所在端 | 用途 |
| --- | --- | --- |
| `8443/tcp` | 控制台 | Web 管理界面，正式环境建议启用 HTTPS |
| `9443/tcp` | 控制台 | 节点主动连接的 mTLS gRPC 端口 |
| `80/tcp` | 边缘节点 | HTTP 流量 |
| `443/tcp` | 边缘节点 | HTTPS/TLS 或国密 TLCP 流量 |

防火墙至少应允许边缘节点到控制台 `9443/tcp` 的出站连接，以及业务需要的 `80/443/tcp` 入站连接。

## 3. 从源码编译

源码编译步骤已移到 [build-from-source.md](build-from-source.md)，本节不再重复。

## 4. 安装边缘节点

### 4.1 完整节点二进制发布包（推荐）

完整节点包包含 `syedge`、`syedge-agent`、`syedge-wrapper`、sudoers、systemd 单元、日志轮转和安装脚本。

```bash
tar -xzf syedge-node-<版本>-linux-<架构>.tar.gz
cd syedge-node-<版本>-linux-<架构>
sudo ./install.sh
```

默认 system 安装根目录由发布包的编译期前缀决定：本地脚本未设置 `SYEDGE_PREFIX` 时为 `/opt/syedge`，GitHub Actions 发布包默认是 `/home/atwasoft/syedge`。如果需要把程序和运行数据放到其他目录，可在首次安装时指定一个由 root 控制的绝对路径：

```bash
sudo ./install.sh --prefix /data/syedge
```

安装脚本会把根目录写入 `/etc/syedge/prefix`，并将程序、版本库、配置、日志、缓存和 agent 状态放到该目录；`/run/syedge*`、`/usr/bin`、`/usr/libexec`、`/usr/sbin` 和 systemd 单元位置不变。重复执行时不带 `--prefix` 会沿用原路径，传入不同路径会拒绝且不迁移已有数据。

建议选择 `/data/syedge`、`/srv/syedge` 或 `/home/<子目录>` 这样的专用目录；不能把 `/usr`、`/opt`、`/var`、`/home` 等系统共享目录本身作为安装根目录，也不能放在 `/root` 下。安装根目录及已存在的父目录必须由 root 所有、不能被组或其他用户写入，也不能是符号链接；唯一例外是 root 所有且带粘滞位的 `/home`（例如 `1777`），此时其下一级目录必须已经存在并属于 root。同时要允许 `syedge` 与 `syedge-agent` 用户逐级穿过目录、读取 `/etc/syedge/prefix`。安装脚本不会修改已有父目录或安装根目录的属主与权限；不满足条件时，请管理员选择受控目录或调整权限后重试。

例如首次安装到 `/home/atwasoft/node`：

```bash
sudo ./install.sh --prefix /home/atwasoft/node
```

路径中尚不存在的目录由安装脚本创建。若 `/home/atwasoft` 已属于普通用户，或者 `/home`、`/home/atwasoft` 对其他用户可写且不满足上述粘滞位例外，需要先由管理员处理目录归属和权限；仅把 `node` 设为 root 所有无法防止可写的父目录替换它。

位于 `/home` 时，新版 systemd 会仅将安装目录映射进服务沙箱，其他家目录仍被隐藏。较旧 systemd 不支持该映射功能，会开放 `/home` 并继续隐藏 `/root` 和 `/run/user`；此时其他家目录仍依赖其文件权限保护。

自定义目录下，手工校验要显式指定前缀、配置文件，并用 `-e stderr` 把错误输出到终端（与独立模式说明中的写法一致，不依赖日志目录位置）；常规重载通过 systemd 保持服务单元中的路径设置：

```bash
R=$(sudo syedge-instance prefix)
sudo "$R/sbin/syedge" -p "$R" -t -c "$R/conf/current/syedge.conf" -e stderr
sudo systemctl reload syedge
```

数据面服务、wrapper 和实例管理工具会显式传入实际运行前缀。Nginx 语义下使用前缀解析的相对路径，例如 `root html;`、`include conf/example.conf;`，按该前缀查找；手工运行时也应像上例一样带 `-p`。共享实例使用共同运行目录，独立实例使用自己的运行目录。重复安装会保留自定义目录 `html/` 中已有的静态文件。

历史默认安装没有标记文件时会继续使用 `/opt/syedge` 与原来的 `/var` 目录；已有节点组的空根目录同样仍表示 `/opt/syedge`。新建节点组记录发行包默认目录。新旧默认目录均存在配置但没有标记时，程序会拒绝自动猜测，须由管理员确认实际安装。

安装脚本会：

- 创建 `syedge` 和 `syedge-agent` 系统用户；
- 安装编译期默认的数据面根目录、Agent、wrapper 和 systemd 单元；
- 初始化基线配置 `r000000`；
- 执行 `syedge -t -q`；
- 启动或平滑 reload `syedge.service`。

检查安装结果：

```bash
syedge -v
syedge -V
sudo syedge -t -q
sudo systemctl status syedge --no-pager
sudo ss -lntp | grep -E ':(80|443)\b' || true
```

### 4.2 安装后注册到控制台

在控制台“节点”页面点击“添加节点”，把一次性注册命令复制到节点上执行。等价命令如下：

```bash
sudo syedge-agent enroll \
  -server <控制台域名或IP>:9443 \
  -token <一次性令牌> \
  -ca-fingerprint <控制台CA的SHA256指纹>

sudo systemctl enable syedge-agent && sudo systemctl start syedge-agent
sudo systemctl status syedge-agent --no-pager
```

也可以在安装时一次完成：

```bash
sudo ./install.sh \
  --server <控制台域名或IP>:9443 \
  --token <一次性令牌> \
  --ca-fingerprint <控制台CA的SHA256指纹>
```

如果节点原来是独立模式并且已有人工维护的配置，注册默认会拒绝，以防控制台首次发布静默覆盖本机配置。确认由控制台接管时使用：

```bash
sudo syedge-agent enroll \
  -server <控制台>:9443 \
  -token <一次性令牌> \
  -ca-fingerprint <指纹> \
  -replace-local-config
```

原配置会备份到 `/var/lib/syedge-agent/backups/`，并在控制台首次发布前继续运行。

### 4.3 RPM/DEB 安装

RPM 系统：

```bash
sudo mkdir -p /etc/syedge
printf '%s\n' /data/syedge | sudo tee /etc/syedge/prefix
sudo chmod 0644 /etc/syedge/prefix
sudo dnf install ./syedge-<版本>*.rpm ./syedge-agent-<版本>*.rpm
sudo systemctl enable syedge && sudo systemctl start syedge
```

Debian/Ubuntu：

```bash
sudo mkdir -p /etc/syedge
printf '%s\n' /data/syedge | sudo tee /etc/syedge/prefix
sudo chmod 0644 /etc/syedge/prefix
sudo apt install ./syedge_<版本>*.deb ./syedge-agent_<版本>*.deb
sudo systemctl enable syedge && sudo systemctl start syedge
```

首次安装后，按照“4.2 安装后注册到控制台”注册 Agent。`syedge-agent` 软件包依赖 `syedge` 和 `sudo`；可选的 `syedge-firewall` 需要额外安装 `nftables`。

不需要自定义目录时无需创建该标记文件。RPM/DEB 的 payload 保留一份编译期默认目录中的静态文件；postinstall 会将所需程序和默认文件同步到实际安装根目录。安装后不要手工修改标记：`syedge-instance prefix` 不支持迁移，直接修改文件也不会迁移已有数据，可能使服务找不到原配置。

### 4.4 裸数据面二进制安装

`syedge-<版本>-<架构>.tar.gz` 只包含数据面目录树，不包含 Agent、wrapper 和完整节点安装逻辑。它适合调试或制作自有发行包，不能等同于完整节点发布包。

解压数据面包：

```bash
sudo tar -C / -xzf syedge-1.30.6-x86_64.tar.gz
sudo ln -sfn /opt/syedge/sbin/syedge /usr/sbin/syedge
```

如果要在 systemd 节点上运行，还必须安装与该版本匹配的 Agent、wrapper、sudoers 和服务文件。最简单的做法是改用完整节点包；手工组装时至少需要：

```bash
sudo install -D -m 0755 bin/syedge-agent /usr/bin/syedge-agent
sudo install -D -m 0755 bin/syedge-wrapper /usr/libexec/syedge/syedge-wrapper
sudo install -D -m 0644 dataplane/packaging/systemd/syedge.service \
  /etc/systemd/system/syedge.service
sudo install -D -m 0644 dataplane/packaging/systemd/syedge-agent.service \
  /etc/systemd/system/syedge-agent.service
sudo install -D -m 0440 dataplane/packaging/sudoers/syedge-agent \
  /etc/sudoers.d/syedge-agent
```

然后创建运行目录并生成基线配置：

```bash
sudo groupadd -r syedge 2>/dev/null || true
sudo useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge 2>/dev/null || true
sudo useradd -r -g syedge -d /nonexistent -s /usr/sbin/nologin syedge-agent 2>/dev/null || true
sudo mkdir -p /opt/syedge/conf/releases /var/log/syedge /var/cache/syedge /run/syedge /var/lib/syedge-agent /run/syedge-agent
sudo syedge-agent bootstrap
sudo chown -R syedge-agent:syedge /opt/syedge/conf /var/lib/syedge-agent /run/syedge-agent
sudo syedge -t -q
sudo systemctl daemon-reload
sudo systemctl enable syedge && sudo systemctl start syedge
```

手工组装容易出现用户、权限、systemd 和默认配置不一致，生产环境应优先使用完整节点包或 RPM/DEB。

### 4.5 user 模式：不使用 root 安装

没有 root 权限的服务器使用 user 模式节点包 `syedge-node-user-<版本>-linux-<架构>.tar.gz`。所有文件都在一个由当前用户拥有的安装根目录下，不写 `/etc`、`/usr`、`/opt`，也不需要 sudo。设计见 `docs/design/0001-deploy-layout.md` 第 6 节。

```bash
tar -xzf syedge-node-user-<版本>-linux-<架构>.tar.gz
cd syedge-node-user-<版本>-linux-<架构>
./install-user.sh $HOME/syedge --http 8080 --https 8443 --status 8081 \
  --console <控制台>:9443 --token <令牌> --ca-fingerprint <指纹>   # 注册参数可省略，稍后再注册
```

安装后的目录：

| 路径 | 内容 |
| --- | --- |
| `R/bin/` | `syedge-agent`、`syedge-wrapper`、`syedge-instance` |
| `R/sbin/syedge`、`R/modules/` | 数据面与动态模块 |
| `R/conf/`、`R/logs/`、`R/cache/`、`R/run/`、`R/agent/` | 默认实例的配置、日志、缓存、PID 与 agent 状态 |
| `R/instances/<实例>/` | 具名实例（目录结构同上） |

**进程守护**：本机有可用的 `systemd --user` 时，安装程序在 `~/.config/systemd/user/` 写入 `syedge.service`、`syedge-agent.service`（具名实例为 `syedge-<实例>.service` 等）并启用；要开机不登录也启动，还需执行 `loginctl enable-linger`（部分发行版需要管理员授权）。没有 `systemd --user` 时，安装程序会打印两行 crontab，加入 `crontab -e` 即可：每分钟执行一次 `syedge-instance --root R ensure`，只拉起未运行的进程，不会重启正在运行的数据面。

**日常管理**（命令都要带 `--root R`）：

```bash
R=$HOME/syedge
$R/bin/syedge-instance --root $R list
$R/bin/syedge-instance --root $R add blue --http 18080 --https 18443 --status 18081
$R/bin/syedge-instance --root $R remove blue --yes      # 日志移到 R/removed-logs/，加 --purge 一并删除
$R/bin/syedge-wrapper reload                            # 平滑重载默认实例；具名实例加 --instance blue
```

**与 system 模式的差别**：

- 只能监听 1024 及以上的端口，80/443 请由前置负载均衡或防火墙端口转发进来；控制台的节点组实例档案也要使用这些端口，模式选择 user。
- agent 直接调用 `R/bin/syedge-wrapper`（不经过 sudo），wrapper 仍校验参数与升级包签名；wrapper 发现自己以 root 运行时拒绝执行。
- 不配置本机防火墙，不使用系统 logrotate：agent 每 5 分钟检查一次，按天或超过 256 MiB 轮转 `R/logs` 下的日志并 reopen，保留 14 份（gzip 压缩）。
- agent 与数据面 worker 是同一个用户，隔离性弱于 system 模式。面向互联网的生产节点仍推荐 system 模式。
- 卸载：先逐个 `remove` 具名实例，停止默认实例（`$R/bin/syedge-wrapper quit`）与 agent，删除 systemd 用户单元或 crontab 行，再删除整个 `R`。

## 5. 安装控制台

### 5.1 控制台二进制包

```bash
tar -xzf syedge-console-<版本>-linux-<架构>.tar.gz
cd syedge-console-<版本>-linux-<架构>

./syedge-console \
  -listen 0.0.0.0:8443 \
  -grpc-listen 0.0.0.0:9443 \
  -grpc-hosts <控制台域名或IP> \
  -grpc-advertise <控制台域名或IP>:9443 \
  -tls-auto \
  -data ./data
```

生产环境不要把未加密的 Web 控制台暴露到公网。使用 `-tls-auto` 时，首次访问前从 `<数据目录>/pki/web-ca.crt` 或 `https://<控制台>/web-ca.crt` 下载 CA 并导入管理人员浏览器；已有企业证书时使用 `-tls-cert` 和 `-tls-key`。

### 5.2 RPM/DEB 控制台安装

```bash
# RPM
sudo dnf install ./syedge-console-<版本>*.rpm

# Debian/Ubuntu
sudo apt install ./syedge-console_<版本>*.deb
```

编辑 `/etc/syedge-console/console.env`，至少填写控制台证书和节点连接相关配置：

```dotenv
ETOS_LISTEN=127.0.0.1:8443
ETOS_TLS_AUTO=1
ETOS_GRPC_LISTEN=0.0.0.0:9443
ETOS_GRPC_HOSTS=console.example.com
ETOS_GRPC_ADVERTISE=console.example.com:9443
ETOS_DATA=/var/lib/syedge-console
```

然后启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable syedge-console && sudo systemctl start syedge-console
sudo systemctl status syedge-console --no-pager
```

需要 PostgreSQL 时增加 `ETOS_DB`，例如：

```dotenv
ETOS_DB=postgres://edgetraffic:<密码>@127.0.0.1:5432/edgetraffic?sslmode=disable
```

控制台数据目录包含数据库、节点 CA、安装 ID、加密密钥和证书材料，必须纳入备份范围。

## 6. 国密安装与配置

### 6.1 安装国密数据面

先安装标准节点并接入控制台。国密版是数据面变体，需要厂商签名的国密升级包；不是对标准节点包再执行一次安装脚本。厂商对 Release 的 `syedge-<数据面版本>-1-gmssl-<架构>.tar.gz` 签名，客户把包与签名导入控制台“数据面版本库”，在节点组选择 `gmssl` 并发起升级，详见 `docs/gm.md`。已安装 Lua 时，还需导入相同发行版本的 Lua 附加包。

升级器自动使用节点安装时确定的根目录，无需重复输入 `--prefix`。共享实例会和同目录的其他共享实例一起升级；独立实例只升级自己。确认默认或共享实例的国密变体：

```bash
R="$(sudo /usr/sbin/syedge-instance prefix)"
"$R/sbin/syedge" -v
"$R/sbin/syedge" -V 2>&1 | grep enable-ntls
```

确认输出包含 `(gmssl)` 和 `enable-ntls`。控制台根据节点上报自动识别“国密”标签。
独立实例 `blue` 请检查 `"$R/instances/blue/sbin/syedge" -V`。

### 6.2 国密双证书配置

生产环境建议在控制台“证书”页面上传签名证书、签名私钥、加密证书和加密私钥，再在站点 HTTPS 配置中选择国密证书。控制台会校验证书类型、用途、私钥匹配关系和域名覆盖范围。

独立模式下，可在站点配置中使用铜锁支持的 `ssl_conf_command`：

```nginx
server {
    listen 443 ssl;
    server_name www.example.com;

    ssl_certificate     certs/site-gov.crt;
    ssl_certificate_key certs/site-gov.key;
    ssl_ciphers "ECC-SM2-SM4-GCM-SM3:ECC-SM2-SM4-CBC-SM3:ECDHE-SM2-SM4-GCM-SM3:ECDHE-SM2-SM4-CBC-SM3:HIGH:!aNULL:!MD5";

    ssl_conf_command Enable_ntls on;
    ssl_conf_command SignCertificate /opt/syedge/conf/releases/r000123/certs/site-gov.sm2-sign.crt;
    ssl_conf_command SignPrivateKey  /opt/syedge/conf/releases/r000123/certs/site-gov.sm2-sign.key;
    ssl_conf_command EncCertificate  /opt/syedge/conf/releases/r000123/certs/site-gov.sm2-enc.crt;
    ssl_conf_command EncPrivateKey   /opt/syedge/conf/releases/r000123/certs/site-gov.sm2-enc.key;
}
```

注意：

- 标准版 `syedge` 不支持 `Enable_ntls`；配置校验会失败。
- 国密站点的证书路径应指向当前版本目录，不能指向会被下一次发布覆盖的临时目录。
- 有国密站点的节点组只能升级到 `gmssl` 变体，且节点组中的所有节点都必须是国密版。
- 国密能力需要授权功能 `gm_tls`。

## 7. Lua 附加包

### 7.1 安装

Lua 附加包必须与当前运行中的 `syedge` 版本一致。一个附加包同时包含标准版和国密版模块，安装脚本会根据 `syedge -V` 自动选择正确模块。
system 模式自动读取节点安装根目录（包括 `/home/atwasoft/node`），不需要再次输入安装位置。多个实例时安装脚本会让用户选择目标；非交互环境请加 `--instance <实例名>`，默认实例名为 `default`。

```bash
mkdir -p /tmp/syedge-lua
tar -C /tmp/syedge-lua -xzf syedge-lua-1.30.6-x86_64.tar.gz
cd /tmp/syedge-lua
sudo bash install.sh
# 或明确指定实例：sudo bash install.sh --instance blue
```

独立模式（未接入控制台）可以一条命令安装并启用 Lua，不需要手改主配置：

```bash
sudo bash install.sh --standalone          # 安装附加包，并写入主配置的 load_module 与 conf.d/00-lua-standalone.conf（不 reload）
sudo systemctl reload syedge
sudo bash install.sh --standalone-disable  # 停用（不卸载附加包）；卸载前必须先停用
```

前置条件是节点未注册到控制台、`conf/current` 指向初始版本 `r000000`、主配置中没有 `load_module`。启用后自己的 Lua 文件放在 `R/lualib/` 下，站点里用 `require` 加载。详细说明见 [docs/lua.md 独立模式手工 Lua](lua.md)。

安装脚本会：

- 校验附加包版本与运行中的 `syedge` 版本一致；
- 共享实例安装到 `R/modules/lua-<版本>/`，独立实例安装到 `R/instances/<实例>/modules/lua-<版本>/`；
- 原子切换选定模块目录下的 `lua` 软链接；
- 执行真实的 `syedge -t -q` 自检；
- 失败时恢复原来的模块软链接。

安装和卸载都不会重启或停止正在运行的 `syedge`。检查结果：

```bash
R="$(sudo /usr/sbin/syedge-instance prefix)"
readlink -f "$R/modules/lua"
sudo "$R/sbin/syedge" -t -q -c "$R/conf/current/syedge.conf"
sudo systemctl status syedge --no-pager
```

Lua JWT 规则与自定义 Lua 脚本都需要授权功能 `lua`，并且控制台发布前会检查节点组内每台节点都安装了匹配的 Lua 附加包。自定义 Lua 脚本只允许管理员提交、另一位管理员批准，运行在受限环境中（见 `docs/lua.md` 第 5 节）。独立模式的受信任运维人员可以在 `conf.d` 中使用原生 Lua 阶段指令。

### 7.2 卸载

当前配置仍在使用 Lua/JWT 时，卸载脚本会拒绝执行。正确顺序是：

1. 在控制台去掉 Lua/JWT 规则并发布；
2. 确认节点上的 `syedge -t -q` 通过；
3. 执行卸载。

```bash
sudo bash uninstall.sh
```

### 7.3 带 Lua 节点的升级

Lua 模块与 `syedge` 二进制版本强绑定。升级带 Lua 的节点时，必须同时提供同一版本的 `syedge` 和 `syedge-lua` 包；缺少其中任何一个，控制台应拒绝升级。国密 Lua 节点仍需使用 `gmssl` 数据面包，不能换成标准版。

## 8. 配置与日常运维

### 8.1 关键目录和文件

| 路径 | 说明 | 其他安装根目录 `R`（非 `/opt/syedge`） |
| --- | --- | --- |
| `/opt/syedge/sbin/syedge` | 数据面主程序 | `R/sbin/syedge` |
| `/opt/syedge/conf/current` | 当前生效配置版本软链接；控制台托管时由 Agent 管理 | `R/conf/current` |
| `/opt/syedge/conf/releases/` | 配置版本目录 | `R/conf/releases/` |
| `/opt/syedge/share/defaults/` | 默认文件、独立模式示例和基线模板 | `R/share/defaults/` |
| `/opt/syedge/modules/lua` | 当前 Lua 模块版本软链接 | `R/modules/lua` |
| `/var/log/syedge/error.log` | 错误日志 | `R/logs/error.log` |
| `/var/log/syedge/access.json` | 访问日志（JSON）。基线与控制台发布的配置都只写这个文件；`access.log` 是编译期默认路径，只有自带主配置且没有写 `access_log` 的原生兼容配置才会用到 | `R/logs/access.json` |
| `/var/cache/syedge/` | 临时文件目录 | `R/cache/` |
| `/run/syedge/syedge.pid` | master PID 文件 | 同左 |
| `/var/lib/syedge-agent/` | Agent 身份、状态、升级文件和本机配置备份 | `R/agent/` |
| `/var/lib/syedge-console/` | 控制台数据库、CA、安装 ID 和加密密钥 | 同左（与节点前缀无关，由 `ETOS_DATA` 决定） |

控制台托管模式下，不要直接编辑 `/opt/syedge/conf/current`。应在控制台修改、预览、发布；Agent 会执行配置校验、reload、探活和失败回滚。

### 8.2 独立模式配置

只运行数据面、不接入控制台时，安装后的基线配置已经存在：

```bash
sudo systemctl enable syedge && sudo systemctl start syedge
sudo cp /opt/syedge/share/defaults/standalone-example.conf \
  /opt/syedge/conf/current/conf.d/www.conf
sudo vi /opt/syedge/conf/current/conf.d/www.conf
sudo syedge -t
sudo systemctl reload syedge
```

建议把 `syedge -t` 和 reload 连成一次操作：

```bash
sudo syedge -t && sudo systemctl reload syedge
```

未匹配到任何 `server_name` 的请求会按基线配置处理，不要通过删除基线配置来“放开”流量。

### 8.3 手动指定配置文件

`syedge` 保留了上游 Nginx 的启动参数。需要校验、启动或控制某一份指定配置时，使用同一组 `-p` 和 `-c` 参数：

| 参数 | 作用 |
| --- | --- |
| `-p <prefix>` | 设置运行前缀；配置中的相对路径、相对 `include` 路径按该前缀解析 |
| `-c <file>` | 指定配置文件；多实例场景建议使用绝对路径 |
| `-t` | 检查配置语法和文件路径，不启动服务 |
| `-T` | 检查配置并输出展开后的全部配置 |
| `-q` | 配置检查时只输出错误 |
| `-s reload` | 平滑重载指定配置对应的 master |
| `-s reopen` | 重新打开指定配置中的日志文件 |
| `-s quit` | 优雅停止指定配置对应的 master |
| `-g <directives>` | 临时追加全局指令；生产配置不建议依赖此参数 |

例如，配置文件放在 `/etc/syedge/instances/blue/syedge.conf` 时：

```bash
sudo /opt/syedge/sbin/syedge \
  -p /etc/syedge/instances/blue \
  -c /etc/syedge/instances/blue/syedge.conf \
  -t -q

sudo /opt/syedge/sbin/syedge \
  -p /etc/syedge/instances/blue \
  -c /etc/syedge/instances/blue/syedge.conf \
  -s reload
```

`-p` 不会复制或重新安装二进制，只改变本次进程使用的运行前缀；`-c` 也不会自动修改配置中的 `pid`、日志和临时目录。多实例必须在各自配置中显式设置这些路径，不能只靠 `-p` 隔离。

#### 控制台、Agent 和实例管理命令的配置文件

控制台、Agent 和实例管理工具也支持用 `--config <文件>` 或 `-c <文件>` 显式选择配置。它们使用 `KEY=VALUE` 格式，一行一个配置；支持空行、以 `#` 开头的注释及成对的单引号或双引号。配置只作为文本读取，不执行 shell 命令或展开 `$变量`。未知键、重复键和格式错误会报错，程序不会按错误配置启动。文件必须能被运行程序的用户读取，包含数据库口令等敏感信息时应限制文件权限。

同一项的优先级为**命令行参数 > 指定的配置文件 > 原有环境变量 > 程序默认值**。只在配置文件中出现的路径型字段支持以该文件所在目录为基准的相对路径；命令行与环境变量的路径保持原有行为。SQLite 的 `file:` DSN 和 PostgreSQL 连接串保留原样，其内部相对路径仍由数据库驱动处理。未指定 `--config` 时，原有启动命令和 systemd 的 `EnvironmentFile` 行为不变。

例如，可以直接让控制台读取已有的配置文件：

```bash
syedge-console --config /etc/syedge-console/console.env
syedge-console backup --config /etc/syedge-console/console.env -out /备份目录/console.tar.gz
```

备份仍需要通过 `-passphrase-file` 或环境变量 `ETOS_BACKUP_PASSPHRASE` 提供口令。控制台配置文件支持 `ETOS_LISTEN`、`ETOS_TLS_CERT`、`ETOS_TLS_KEY`、`ETOS_TLS_AUTO`、`ETOS_WEB_HOSTS`、`ETOS_GRPC_LISTEN`、`ETOS_GRPC_HOSTS`、`ETOS_GRPC_ADVERTISE`、`ETOS_DATA`、`ETOS_DB` 和 `ETOS_ACME_CA_FILE`。如果由 systemd 管理，并希望启动时使用另一个文件，可用 `systemctl edit syedge-console` 设置以下覆盖项，再按维护窗口重启控制台服务；自定义数据目录还需检查服务用户权限及 `ProtectHome`、`ProtectSystem` 等 systemd 限制：

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/syedge-console --config /etc/syedge-console/custom.env
```

默认节点的 Agent 可使用类似的文件，例如 `/etc/syedge-agent/custom.env`：

```ini
SYEDGE_CONSOLE=10.100.51.79:11443
```

```bash
syedge-agent run --config /etc/syedge-agent/custom.env
# 注册时也可以指定相同配置文件；令牌和指纹仍通过原有参数提供
sudo syedge-agent enroll --config /etc/syedge-agent/custom.env -token <令牌> -ca-fingerprint <指纹>
```

Agent 可配置的键是 `SYEDGE_INSTANCE`、`SYEDGE_ROOT`、`SYEDGE_CONSOLE`、`SYEDGE_STATE_DIR`、`SYEDGE_CONF_ROOT`、`SYEDGE_WRAPPER`，分别对应原有的实例、普通用户安装根目录、控制台地址、状态目录、配置目录和 wrapper 参数。`SYEDGE_ROOT` 只用于普通用户安装模式；system 模式的数据面安装目录仍由 `/etc/syedge/prefix` 决定。实例和目录一般保持默认即可；自行修改目录时，还需保证服务用户权限和 systemd 的可读写范围匹配。若要让 systemd 的默认 Agent 服务使用文件，可用 `systemctl edit syedge-agent`：

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/syedge-agent run --config /etc/syedge-agent/custom.env
```

具名实例的服务为 `syedge-agent@<实例>.service`；原服务的 `-instance` 参数优先于文件中的 `SYEDGE_INSTANCE`，覆盖 `ExecStart` 时应保留该参数。手工运行控制台或 Agent 前，应确认没有启动会占用相同资源的现有进程；systemd 服务的修改只在重启对应服务后生效，不需要重启数据面。

实例管理命令的配置文件用于提供创建实例时的默认参数。例如 `/etc/syedge/blue.env`：

```ini
SYEDGE_HTTP=18080
SYEDGE_HTTPS=18443
SYEDGE_STATUS=8082
SYEDGE_RESOURCE_MODE=independent
```

```bash
sudo syedge-instance --config /etc/syedge/blue.env add blue
# 等价：sudo syedge-instance add blue --config /etc/syedge/blue.env
```

还可设置 `SYEDGE_NOFILE` 和 `SYEDGE_CONSOLE`；需要同时注册时仍通过 `--token`、`--ca-fingerprint` 提供令牌和指纹。`SYEDGE_ROOT` 等同于普通用户模式的 `--root`，不能用来更改 system 模式的安装根目录。创建后的实例档案仍保存在对应的 `instance.json`；修改这个默认参数文件不会自动修改已有实例或其 systemd 服务。Lua、国密是数据面的可选组件，沿用数据面的 `-p` 和 `-c`，不是另外启动的程序。

### 8.4 一台服务器运行多套 syedge（同机多实例）

同一台服务器可以运行多个 syedge 实例，各用不同端口，并分别由控制台托管、发布、热升级和卸载。设计见 `docs/design/0001-deploy-layout.md`。

**前提**：节点已按 8.1 节安装了软件包（默认实例照常存在）。每个具名实例对应控制台中一个**节点组**，节点组的“实例档案”（实例名与端口）决定该组所有节点上的路径与监听端口；同一台服务器上的实例必须属于不同的节点组。

**1. 在控制台设置节点组的实例档案**（`PUT /api/v1/node-groups/<id>/profile`），例如实例名 `blue`、HTTP 18080、HTTPS 18443、状态页 8082，并选择数据面与模块“共享”或“独立”。组内已有节点后实例名与共享方式不能再改，端口可以改。控制台会检查同一台服务器上的实例名与端口（含四层代理端口）是否冲突。已有实例保持共享；独立方式只用于新建具名实例。

**2. 在节点上创建实例并注册**：在该节点组的“添加节点”中生成命令，形如：

```bash
sudo syedge-instance add blue --http 18080 --https 18443 --status 8082 \
  --resource-mode independent \
  --console <控制台>:9443 --token <令牌> --ca-fingerprint <指纹>
```

它会依次：检查端口未被占用且不与本机其他实例冲突；创建该实例专用的 agent 用户 `syedge-agent-blue` 与只允许操作本实例的 `/etc/sudoers.d/syedge-agent-blue`（经 `visudo` 校验）；创建目录与 `/opt/syedge/instances/blue/instance.json`；生成初始配置并用 `syedge -t` 校验；启动 `syedge@blue.service`；注册并启动 `syedge-agent@blue.service`。不带 `--console` 等参数时只创建并启动数据面，输出稍后注册的命令。

| 项目 | 默认实例 | 具名实例 `blue` |
| --- | --- | --- |
| 配置 | `/opt/syedge/conf/current/syedge.conf` | `/opt/syedge/instances/blue/conf/current/syedge.conf` |
| PID | `/run/syedge/syedge.pid` | `/run/syedge-blue/syedge.pid` |
| 日志 / 缓存 | `/var/log/syedge/`、`/var/cache/syedge/` | `/var/log/syedge-blue/`、`/var/cache/syedge-blue/` |
| agent 状态 | `/var/lib/syedge-agent` | `/var/lib/syedge-agent-blue` |
| 服务 | `syedge.service`、`syedge-agent.service` | `syedge@blue.service`、`syedge-agent@blue.service` |

省略 `--resource-mode` 或指定 `shared` 时使用共享二进制和模块；指定 `independent` 时，程序、版本库和模块分别位于 `R/instances/blue/sbin/`、`versions/`、`modules/`，可独立选择 Lua 和国密变体。

**3. 日常管理**

```bash
sudo syedge-instance list                  # 实例、端口与数据面运行状态
sudo syedge-instance check                 # 端口 / 名称冲突与缺失文件
sudo systemctl reload syedge@blue          # 平滑重载单个实例（不影响其他实例）
sudo syedge-instance remove blue           # 只显示将执行的操作
sudo syedge-instance remove blue --yes     # 优雅停止并删除实例（保留日志；加 --purge 一并删除日志与缓存）
```

删除实例后，请在控制台“节点”页吊销对应节点；不再使用的节点组可随后删除（见下）。日志轮转（logrotate）会对所有实例执行 `syedge-instance reopen-all`。

**卸载**：本机还有具名实例时，卸载软件包（`rpm -e` / `apt remove`）与 `uninstall.sh` 会被拒绝；请先逐个 `syedge-instance remove`，或执行 `uninstall.sh --all`。自定义 system 根目录下删除单个实例时，日志和缓存默认移到 `R/removed-instances/<实例>-*/`，加 `--purge` 才一并删除。节点包的 `uninstall.sh --purge` 只清理节点使用的目录，保留根目录下其他文件，例如控制台的 `data/`。软件包升级不会停止任何实例。

**控制台中的同机多实例**：

- 节点组的生命周期都在“节点”页完成：“新建节点组”（管理员）→“节点组设置”中改名、修改说明与回源网段、编辑实例档案（操作员）→“节点组设置 → 删除节点组…”（管理员）。
  删除前会列出将随组删除的站点、上游、版本与发布记录等；组内还有未吊销的节点、有进行中的发布 / 审批 / 灰度 / 升级，或者它是最后一个节点组时不能删除；需输入组名确认。删除只在控制台侧进行，不向节点下发任何命令，证书库与审计日志不受影响。
- 节点组的实例档案在“节点 → 节点组设置 → 实例档案”中编辑。组内已有节点后实例名、运行方式与安装目录不能再改，端口可以改（改后需在各节点上按新端口重建实例）。
- “节点”页可切换为“按主机”查看：同一台服务器上的实例、各自的端口与实际运行的数据面版本；实例版本不一致时会标出（同时有“同机实例版本不一致”告警规则）。
- 数据面升级以主机为单位：升级某个节点组时，这些节点所在主机上其他节点组的实例会随主机一起升级（本组实例先、同机实例后，依次热升级）。同机第二个实例失败时，该实例回到旧 master，主机标为“部分升级”，升级记录中提供“重试未完成的实例”和“主机整体回退”。任何实例都不会停止。
- 节点防火墙（8.9 节）按全部实例当前配置中的端口放行，实例增删与每次发布后自动刷新。

### 8.5 与 Nginx 配置的差异

`syedge` 的数据面基于 NGINX Open Source 源码并采用改名 patch，因此 `http`、`events`、`stream`、`server`、`location`、`proxy_pass`、大部分 `ssl_*` 指令和配置文件语法与 Nginx 一致。但它不是可以无条件替换 Nginx 二进制的“同名安装”，主要差异如下：

| 项目 | Nginx 常见方式 | syedge 方式 |
| --- | --- | --- |
| 程序名 | `nginx` | `syedge` |
| 常见配置入口 | `/etc/nginx/nginx.conf` | `/opt/syedge/conf/current/syedge.conf`；手工实例可用 `-c` 指定 |
| 配置检查 | `nginx -t` | `syedge -t` 或 `systemctl reload syedge` 前的 `ExecStartPre` |
| 平滑操作 | `nginx -s reload` | `syedge -s reload` 或 `systemctl reload syedge` |
| 默认运行目录 | 由发行版打包方式决定 | `/opt/syedge`、`/run/syedge`、`/var/log/syedge`、`/var/cache/syedge` |
| 配置管理 | 通常由管理员直接编辑 | 控制台托管时由 Agent 按版本目录发布，手改会被标记漂移或被下一次发布替换 |
| 配置版本 | 常见为原地覆盖 | `/opt/syedge/conf/releases/<revision>` + `conf/current` 软链接 |
| Lua | 取决于 Nginx 构建和第三方模块 | 使用版本绑定的 `syedge-lua` 附加包，不能随意复制其他 Nginx 的 `.so` |
| 国密 | 取决于额外国密 patch/发行版 | 使用 `-t tongsuo` 构建的 `gmssl` 变体，并通过 `ssl_conf_command` 配置 NTLS |

当前产品构建还明确启用了 `ssl`、HTTP/2、HTTP/3、`realip`、`stub_status`、`gzip_static`、`sub`、`stream`、`stream_ssl`、`stream_ssl_preread`、`auth_request` 等模块，并关闭了 `http_auth_basic`。需要 `auth_basic` 的旧 Nginx 配置不能直接迁移，应改用产品支持的鉴权方式或重新评估构建选项；最终以 `syedge -V` 输出的 configure 参数为准。

产品基线配置也不同于发行版的默认 Nginx 配置：它自带 JSON 访问日志、请求头基线、限速区域、本机 `stub_status` 状态页，以及未匹配域名直接断开的默认 server。迁移 Nginx 配置时，建议把业务 `server/upstream/location` 逐项迁移到 `conf.d`，不要用旧的 `/etc/nginx` 目录整体覆盖 `/opt/syedge/conf/current`。

**通过控制台迁移**：在“配置导入”中选择目标节点组，上传一份 `nginx -T` 输出、配置目录的 `tar.gz`，或一次选择主配置及其引用的多个文件。导入器只读取上传内容，按原 `include` 所在的 `http`、`server` 或 `location` 上下文展开；没有提供的文件会在报告中提示。单独选择多个文件时，应避免同名文件；需要保留目录或通配符路径时，使用 `tar.gz` 或 `-T` 输出。

上传后先核对导入报告，再点击“应用为草稿”。不支持的指令、未找到的 `include`、Lua 脚本和本机静态文件需要人工处理；HTTPS 证书和私钥没有包含在 `-T` 输出中，需要在“证书”页上传。报告中的站点可能因未完成转换而停用，应用草稿不等于发布。已有草稿不会被重新导入覆盖，重复导入可能产生同域名草稿，发布前应先整理。完成检查后再预览、审批并发布。

导入报告可按证书、静态资源、上游、WebSocket、Lua、缺失文件等类型筛选。应用后可从报告进入证书库、上游、片段或直接编辑对应站点，并使用“重新校验草稿”查看当前模型校验和片段审批状态。历史导入记录若未保存站点标识，请在“站点与上游”中按名称核对；不要仅凭同名推断站点身份。页面保留的是导入分析结果而非原始配置全文，路由对照只显示已识别结构的摘要，未转换的条目不会因模型校验通过而自动消失。草稿停用时不会进入发布预览；节点发布时才执行实际的配置语法校验。

已有独立配置的节点可先把节点组切换为“观察接入”或“兼容配置”，再按控制台生成的命令注册。Agent 会在首次注册时把现有配置备份到 `agent/backups/standalone-*.tar.gz`；这两种模式不会在节点上线时自动对齐，也会拒绝发布、审批下发、灰度、回滚、漂移恢复和证书续期下发。导入报告会额外列出原生 Lua 指令、上传包中发现的脚本/依赖路径、大小与 SHA-256；它们不会被执行或自动转成托管脚本。确认迁移结果后，在节点组设置中切回“控制台托管”，再人工预览、审批和发布；切回本身不会自动覆盖现网。完整流程见 [现有配置的观察接入与 Lua 兼容](legacy-config-management.md)。

静态目录需另行打包并在对应站点的静态路由中上传；原 `root`、`alias`、`try_files` 的访问路径和行为需要逐项核对。代理路由可启用标准 WebSocket 连接升级；导入器仅在源路由明确使用 HTTP/1.1、`Upgrade $http_upgrade` 和 `Connection upgrade` 且没有冲突时自动转换，其他头、动态变量和超时需手工适配。Lua 脚本及依赖不会自动迁移：需要相应授权、节点 Lua 扩展包和另一人审批，并在受限运行环境中验证兼容性。

从 OpenResty 迁移的指令与模块差异、Lua 边界和基线默认值差异，见 [从 OpenResty 迁移](migrate-from-openresty.md)。

### 8.6 类 Nginx 的服务命令

```bash
# 查看状态
sudo systemctl status syedge --no-pager

# 配置语法和路径校验
sudo syedge -t
sudo syedge -T       # 需要查看完整配置展开结果时使用

# 平滑重载：不停止 master，不主动中断现有连接
sudo systemctl reload syedge
# 或
sudo syedge -s reload

# 重新打开日志文件
sudo syedge -s reopen

# 优雅停止
sudo systemctl stop syedge
# 或
sudo syedge -s quit

# 查看版本和编译参数
syedge -v
syedge -V
```

线上配置变更优先使用 `reload`，不要用 `restart` 代替 reload。`restart` 会停止并重新启动服务，可能中断现有连接。`systemctl stop` 会等待现有连接完成，但等待时间受 systemd 的 `TimeoutStopSec` 限制；有长连接的环境可按需配置服务覆盖文件。

升级前已生成的 user 模式 systemd 单元不会随二进制自动改写。要让这些现有单元也优雅停止，可对每个数据面单元执行 `systemctl --user edit syedge.service`（具名实例替换为 `syedge-<实例名>.service`），添加：

```ini
[Service]
KillMode=mixed
KillSignal=SIGQUIT
```

### 8.7 查看日志

```bash
sudo journalctl -u syedge -n 100 --no-pager
sudo journalctl -u syedge-agent -n 100 --no-pager
sudo journalctl -u syedge-agent -f

sudo tail -f /var/log/syedge/error.log
sudo tail -f /var/log/syedge/access.json
```

日志由 `/etc/logrotate.d/syedge` 每日轮转，默认保留 14 份并压缩。手工轮转或更换日志文件后执行：

```bash
sudo logrotate -f /etc/logrotate.d/syedge
sudo syedge -s reopen
```

### 8.8 Agent 和控制台维护

```bash
# Agent
sudo systemctl status syedge-agent --no-pager
sudo systemctl restart syedge-agent
sudo syedge-agent version

# 控制台
sudo systemctl status syedge-console --no-pager
sudo journalctl -u syedge-console -f
sudo systemctl restart syedge-console
```

重启 Agent 不会重启数据面；重启控制台也不会停止边缘节点上的 `syedge` 流量处理。

### 8.9 可选节点防火墙

节点安装 `nftables` 后，可启用产品自己的独立 nftables 表：

```bash
sudo syedge-firewall enable \
  --ssh-from 10.0.0.0/8,192.0.2.10 \
  --allow-tcp 9100 \
  --allow-udp 443

sudo syedge-firewall status
sudo syedge-firewall disable
```

启用前确认当前 SSH 来源包含在 `--ssh-from` 中，否则工具会拒绝执行；确实需要强制启用时才使用 `--force`。如果同时运行 firewalld 或 ufw，还要在对应防火墙中放行业务端口。

放行的端口来自本机全部实例当前生效的配置（各实例的 HTTP/HTTPS 端口与四层代理端口，只监听 127.0.0.1 的状态页除外），读不到实例配置时为 80/443；`--allow-tcp` / `--allow-udp` 在此之外追加。以下情况会自动刷新规则，未启用防火墙时什么都不做：

- `syedge-instance add` / `remove` 之后；
- 每次发布之后（`syedge-firewall-refresh.path` 监视各实例的配置目录，发布切换 `current` 时触发）；
- 手工执行 `sudo syedge-firewall refresh`。

刷新时如果某个实例的配置监听了 SSH 端口，会拒绝生成并保留现有规则（日志见 `journalctl -u syedge-firewall-refresh`）。从旧版本升级后需执行一次 `enable`（参数同前），之后才会自动刷新。

### 8.10 运行身份：运行时不使用 root

安装、卸载与 `syedge-instance` 等管理命令需要 root；运行时的进程都不是 root：

| 进程 | 运行身份 | 能力 |
| --- | --- | --- |
| `syedge` master 与 worker（`syedge.service`、`syedge@<实例>.service`） | `syedge` | 只有 `CAP_NET_BIND_SERVICE`（绑定 80/443） |
| `syedge-wrapper`（agent 经 sudo 调用，sudoers 为 `(syedge)`） | `syedge` | 文件能力 `cap_net_bind_service=ip`，只允许 `syedge` 组执行 |
| `syedge-agent` | `syedge-agent`、`syedge-agent-<实例>` | 无 |
| 防火墙开机加载与自动刷新 | `syedge-firewall` | 只有 `CAP_NET_ADMIN` |

检查：

```bash
ps -C syedge -o user=,args=                         # 全部为 syedge
grep CapEff /proc/$(cat /run/syedge/syedge.pid)/status   # 0000000000000400（只有 CAP_NET_BIND_SERVICE）
getcap /usr/libexec/syedge/syedge-wrapper            # cap_net_bind_service=ip
```

从旧版本升级时，运行中的 master 仍以 root 运行。安装脚本与软件包会自动执行 `sudo syedge-instance nonroot`，用与二进制热升级相同的方式（USR2）把它无中断地切换为 `syedge` 运行，不重启、不断开连接。该命令可以重复执行；单独替换了 wrapper 文件（会丢失文件能力）后也应执行一次。

其他变化：

- 日志、缓存、`/run/syedge*`、`/opt/syedge/versions` 归 `syedge`；logrotate 以 `syedge` 身份轮转并重新打开日志。
- 节点防火墙的规则与参数从 `/etc/syedge/firewall.nft` 移到 `/var/lib/syedge-firewall/`（升级时自动迁移）。
- 手工校验配置请以 `syedge` 身份经 wrapper 执行：`sudo -u syedge /usr/libexec/syedge/syedge-wrapper test <版本号>`；以 root 直接执行 `syedge -t` 可能创建属于 root 的日志文件，导致 master 无法写入。
- 配置中不再有 `user` 指令（worker 与 master 同为 `syedge`）。

#### 兼容模式：master 以 root 运行（与 nginx 相同）

非 root 的 master 绑定 80/443 依赖 ambient 能力：内核 4.3 起、systemd 229 起。CentOS 7（内核 3.10、systemd 219）等系统不满足，
`syedge-instance nonroot`（安装与软件包升级时自动执行）会自动进入兼容模式；任何系统也可以用 `./install.sh --root-master`
（或 `syedge-instance nonroot --root-master`）选择它。兼容模式与 nginx 的做法相同：

| 进程 | 运行身份 |
| --- | --- |
| `syedge` master | `root`（绑定端口后按编译进二进制的默认用户启动 worker，配置中仍不写 `user` 指令） |
| `syedge` worker | `syedge`（处理流量的进程不是 root） |
| `syedge-wrapper` | `root`（sudoers 为 `(root)`，仍只有 7 行；向 root master 发信号需要 root） |
| `syedge-agent` | `syedge-agent`（不变） |
| 防火墙开机加载与自动刷新 | `root` |

模式记录在 `/etc/syedge/root-master`（root 所有、他人不可写，否则不被信任）。单元不用附加配置，而是由软件包单元生成完整的
覆盖单元 `/etc/systemd/system/syedge.service`、`syedge@.service` 与两个防火墙单元（只替换运行身份与能力相关的行，第一行注明
“syedge-instance nonroot 生成”）：systemd 229 以下（如 CentOS 7.0/7.1 的 208）对多行 `CapabilityBoundingSet=` 取交集，
附加配置无法放宽软件包单元的能力边界，master 会缺少 `CAP_SETGID`，worker 报 `setgid(...) failed (1: Operation not permitted)`。
覆盖单元在每次 `syedge-instance nonroot`（软件包安装与升级、`install.sh`）时按软件包单元重新生成；v0.4.1 写入的
`*.service.d/root-master.conf` 会被自动删除。热升级、多实例、reload、日志轮转的用法都不变。

**systemd 208（CentOS 7.0/7.1 未更新）**：建议先 `yum update` 到 CentOS 7.9（systemd 219）。不能更新时也可以使用：
`systemctl enable --now` 一律拆成 `enable` 与 `start`；208 不支持的 `RuntimeDirectory=` 由 `syedge-instance` 写入的
`/etc/tmpfiles.d/syedge-instances.conf`（各实例的 `/run/syedge-<实例>` 与 `/run/syedge-agent[-<实例>]`）在开机时重建；
`ProtectHome=`、`StateDirectory=` 等不认识的指令只在日志中警告，不影响启动。在 v0.4.1 上手工写的
`/etc/systemd/system/syedge@<实例>.service`、`/etc/sudoers.d/syedge-notty` 与手工创建的 `/run` 目录，升级后请删除前两项
（实例单元会覆盖模板生成的兼容单元），再执行一次 `syedge-instance nonroot`。
检查：`ps -C syedge -o user=,args=` 中 master 为 root、worker 为 syedge。

## 9. 升级、回滚与备份

### 9.1 普通软件包升级

RPM：

```bash
sudo dnf upgrade ./syedge-<新版本>*.rpm ./syedge-agent-<新版本>*.rpm
```

Debian/Ubuntu：

```bash
sudo apt install ./syedge_<新版本>*.deb ./syedge-agent_<新版本>*.deb
```

软件包安装脚本会保留 `/opt/syedge/conf`，升级时对运行中的 `syedge` 执行 reload，不主动 restart。新二进制文件写入后，生产环境的数据面版本切换应使用控制台的数据面热升级流程；单纯重新安装文件不等同于已经替换当前运行中的 master 二进制。

### 9.2 控制台数据面热升级

生产升级建议使用控制台“数据面版本库”与节点组升级流程：

1. 厂商对 `syedge` 升级包签名；
2. 有 Lua 的节点同时导入同版本 `syedge-lua` 包；
3. 有国密站点的节点组只选择 `gmssl` 版本；
4. 先灰度少量节点，再分批升级；
5. 观察节点在线状态、配置版本、业务探活和错误日志；
6. 异常时在版本历史中回滚。

该流程通过 wrapper 校验签名、校验配置并执行二进制热升级；失败时会恢复旧模块软链接和旧 master，避免业务中断。

旧 master 若启动时没有显式指定 `-p`，而新二进制的编译前缀又与当前安装目录不同，程序会拒绝热升级并保留现有服务。请先确认生成的服务单元使用正确的 `-p`，在维护窗口按正常服务流程重启一次；随后才可热升级到不同编译前缀的版本。

### 9.3 控制台备份与恢复

备份口令不要写在命令行上，也不要与备份文件放在同一位置：

```bash
sudo install -m 0600 /dev/null /root/etos-backup.pass
sudo vi /root/etos-backup.pass

sudo syedge-console backup \
  -data /var/lib/syedge-console \
  -passphrase-file /root/etos-backup.pass \
  -out /backup/console-$(date +%F).etosbk
```

恢复必须在控制台未运行时执行：

```bash
sudo systemctl stop syedge-console
sudo syedge-console restore \
  -data /var/lib/syedge-console-new \
  -passphrase-file /root/etos-backup.pass \
  -in /backup/console-<日期>.etosbk
```

备份包含数据库、节点 CA、加密密钥和安装 ID。备份文件和口令必须分开保存；丢失口令无法解密备份。

### 9.4 节点配置备份

接管独立模式配置时，Agent 会自动把当前配置备份到 `/var/lib/syedge-agent/backups/`。在手工维护独立模式前，也建议保存一份：

```bash
sudo tar -C /opt/syedge/conf/current \
  -czf /var/lib/syedge-agent/backups/standalone-manual-$(date +%F-%H%M%S).tar.gz .
sudo chmod 0600 /var/lib/syedge-agent/backups/standalone-manual-*.tar.gz
```

其中可能包含证书私钥，备份文件必须按敏感数据管理。

## 10. 故障排查

### 10.1 `syedge -t` 失败

按以下顺序检查：

```bash
sudo syedge -t
readlink -f /opt/syedge/conf/current
ls -la /opt/syedge/conf/current
ldd /opt/syedge/sbin/syedge | grep 'not found' || true
```

常见原因：

- `conf/current` 不存在或软链接指向不存在的版本目录；
- 站点配置语法错误、证书路径错误或权限不足；
- Lua 配置存在但没有安装匹配版本的 `syedge-lua`；
- 国密配置加载到了标准版 `syedge`；
- `/run/syedge`、日志或缓存目录不存在；
- 二进制架构不匹配或 glibc 版本低于构建要求。

### 10.2 节点在控制台离线

```bash
sudo systemctl status syedge-agent --no-pager
sudo journalctl -u syedge-agent -n 200 --no-pager
sudo getent hosts <控制台域名>
sudo ss -ntp | grep ':9443'
```

重点检查控制台 `ETOS_GRPC_HOSTS`、节点注册时的 `-server`、CA 指纹、DNS 和到 `9443/tcp` 的出站访问。Agent 本身不监听端口，不要通过扫描节点入站端口判断 Agent 是否正常。

### 10.3 国密握手失败

先确认构建变体：

```bash
syedge -v
syedge -V | grep enable-ntls
```

然后检查：

- 双证书是否分别为签名证书和加密证书；
- 签名私钥、加密私钥是否与对应证书匹配；
- `Enable_ntls`、`SignCertificate`、`SignPrivateKey`、`EncCertificate`、`EncPrivateKey` 是否齐全；
- 当前节点组是否全部为 `gmssl` 变体；
- 客户端是否支持 TLCP/NTLS。普通 OpenSSL 客户端不一定支持国密握手，应使用具备 NTLS 能力的铜锁客户端进行验证。

### 10.4 Lua 模块加载失败

```bash
readlink -f /opt/syedge/modules/lua
ls -l /opt/syedge/modules/lua/
syedge -v
sudo syedge -t -q
```

确认附加包版本与 `syedge -v` 中的数据面版本完全一致。不要把其他版本或其他架构的 `.so` 文件直接复制到当前模块目录。卸载前必须先发布不再使用 Lua 的配置，否则 `uninstall.sh` 会拒绝卸载。

### 10.5 控制台无法访问

```bash
sudo systemctl status syedge-console --no-pager
sudo journalctl -u syedge-console -n 200 --no-pager
sudo ss -lntp | grep -E ':(8443|9443)\b'
sudo sed -n '1,160p' /etc/syedge-console/console.env
```

检查 `ETOS_GRPC_HOSTS` 是否已填写、证书中的主机名是否与节点连接地址一致、Web 是否启用了正确的 TLS 证书，以及系统防火墙是否放行 `9443`。

## 11. 生产上线检查清单

- [ ] 已校验发布包 SHA256，并确认 CPU 架构。
- [ ] `syedge -v`、`syedge -V` 与目标版本/变体一致。
- [ ] `sudo syedge -t -q` 通过。
- [ ] `syedge.service`、`syedge-agent.service` 已设置开机启动。
- [ ] 节点能主动连通控制台 `9443/tcp`。
- [ ] 控制台已启用 HTTPS，Web 端口未直接暴露在不可信网络。
- [ ] 国密站点使用 `gmssl` 节点，并完成双证书握手验证。
- [ ] Lua 节点已安装同版本 `syedge-lua`，且 `syedge -t -q` 通过。
- [ ] 已验证 reload、日志 reopen、业务探活和版本回滚。
- [ ] 控制台数据目录、节点证书和备份口令已按分离原则保存。

相关专项说明：

- [国密 TLCP 说明](gm.md)
- [Lua 访问规则与附加包说明](lua.md)
- [控制台高可用说明](ha.md)

### 整合包（syedge-all）

发布页的 `syedge-all-<版本>-linux-<架构>.tar.gz` 是节点包加国密版二进制和 Lua 附加包，首次安装时选变体，不用再分别下载、核对版本：

```bash
tar -xzf syedge-all-<版本>-linux-<架构>.tar.gz && cd syedge-all-<版本>-linux-<架构>
sudo ./install.sh                    # 标准版 + Lua 附加包（默认）
sudo ./install.sh --gmssl            # 国密版 + Lua 附加包
sudo ./install.sh --no-lua           # 不装 Lua 附加包
sudo ./install.sh --lua-standalone   # 不接入控制台：一步启用独立模式 Lua 并 reload
```

- 变体只在安装时选择；重复执行只更新文件，运行中的 master 不重启，新二进制在下次二进制热升级时生效。
- 已装标准版的节点换国密版，用控制台的数据面热升级，不靠重装。
- 整合包只用于首次安装与手动升级；经控制台升级仍使用厂商签名的单独升级包，整合包不进入验签流程。
- 结构与参数边界由 `scripts/ci/all-package.sh` 覆盖；真实安装由 release 工作流在正式标签上冒烟（首次引入，暂不阻塞发布）。
