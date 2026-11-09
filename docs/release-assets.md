## 下载哪个文件

文件名中的 `amd64` / `x86_64` 指 Intel / AMD 64 位，`arm64` / `aarch64` 指 ARM 64 位（鲲鹏、飞腾等）；同一个包按架构各有一份。
安装与升级步骤见仓库 `docs/install-maintenance.md`。

### 交付客户：控制台（装在管理服务器上，任选一种）

| 文件 | 格式 | 适用 |
|---|---|---|
| `syedge-console_<版本>_amd64.deb` / `_arm64.deb` | deb 软件包 | Ubuntu、Debian、统信 UOS |
| `syedge-console-<版本>-1.x86_64.rpm` / `.aarch64.rpm` | rpm 软件包 | CentOS、RHEL、麒麟 V10、openEuler |
| `syedge-console-<版本>-linux-amd64.tar.gz` / `-linux-arm64.tar.gz` | 压缩包，解压即用 | 任意 Linux |
| `syedge-console-<版本>-windows-amd64.zip` / `-windows-arm64.zip` | 压缩包 | Windows |
| `syedge-console-<版本>-darwin-amd64.tar.gz` / `-darwin-arm64.tar.gz` | 压缩包 | macOS（演示、测试） |

控制台也可以直接用镜像 `ghcr.io/xiaoguan521/edgetraffic-os/console:<版本>`。

### 交付客户：边缘节点首次安装（装在每台 syedge 服务器上，三种方式任选一种）

| 方式 | 文件 | 说明 |
|---|---|---|
| 软件包 | `syedge_<版本>_amd64.deb` + `syedge-agent_<版本>_amd64.deb`（arm64 同理） | deb 系统；`syedge` 是数据面，`syedge-agent` 是接入控制台的节点代理与 wrapper |
| 软件包 | `syedge-<版本>-1.x86_64.rpm` + `syedge-agent-<版本>-1.x86_64.rpm`（aarch64 同理） | rpm 系统 |
| 一键安装（root） | `syedge-node-<版本>-linux-amd64.tar.gz` / `-linux-arm64.tar.gz` | 解压后执行 `install.sh` |
| 免 root 安装 | `syedge-node-user-<版本>-linux-amd64.tar.gz` / `-linux-arm64.tar.gz` | 普通用户解压后执行 `install-user.sh`，装在用户目录，只能使用 1024 以上端口（见包内 README.txt） |

### 交付客户：经控制台统一升级 / 扩展（先在离线签名机上签名，再导入控制台“版本库”）

| 文件 | 内容 | 签名参数（`syedge-license-signer sign-package`） |
|---|---|---|
| `syedge-<数据面版本>-amd64.tar.gz` / `-arm64.tar.gz` | 标准版数据面升级包 | `-name syedge -variant standard` |
| `syedge-<数据面版本>-1-gmssl-amd64.tar.gz` / `-arm64.tar.gz` | 国密版数据面升级包（铜锁，国密 TLCP，见 `docs/gm.md`） | `-name syedge -variant gmssl` |
| `syedge-lua-<数据面版本>-x86_64.tar.gz` / `-aarch64.tar.gz` | Lua 附加包，标准版与国密版通用（见 `docs/lua.md`） | `-name syedge-lua -variant lua` |
| `syedge-agent-<版本>-amd64.tar.gz` / `-arm64.tar.gz` | agent 升级包 | `-name syedge-agent` |

签名时另加 `-version <包的版本>` 与 `-arch amd64|arm64`（文件名为 `x86_64` / `aarch64` 的也写 `amd64` / `arm64`）。
数据面版本跟随上游源码（如 `1.30.6`），与产品版本 `<版本>` 无关。国密包的发行版本如 `1.30.6-1`，签名参数 `-version` 必须完整包含 `-1`；已装 Lua 的节点还需导入相同发行版本的 Lua 附加包。

### 只给厂商内部，不要发给客户

| 文件 | 内容 | 放在哪 |
|---|---|---|
| `syedge-vendor-tools-<版本>-<系统>-<架构>` | 离线签名工具 `syedge-license-signer` 与运维手册 | 离线签名机（`docs/license-ops.md`） |
| `syedge-license-portal-<版本>-linux-amd64.tar.gz` / `-arm64.tar.gz` | 授权后台（客户、订单、审批、签发记录），只持有公钥 | 厂商内网服务器 |

`SHA256SUMS` 是全部文件的校验和（`sha256sum -c SHA256SUMS --ignore-missing`），可以与客户文件一起提供。
