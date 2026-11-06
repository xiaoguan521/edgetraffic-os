# 正式授权运维手册（License 签名密钥与离线签发）

对应规格书 8.8.1。本手册面向厂商内部：负责人、签名员、商务。客户不需要阅读。

## 1. 原则

- **License 签名私钥只存在于一台物理断网的离线签名机上**，以口令加密的文件形式保存。它不进入云端、CI、代码仓库、授权后台，也不通过网络传输。
- 在线侧（授权后台，或它上线前的 `syapp-license-server` 命令）只持有公钥：生成“签名请求”、核对“签名结果”。
- 两侧之间只用 U 盘传递两种文件：`sign-request-<批次>.json`（待签内容，不含密钥）与 `signed-<批次>.json`（签名结果）。
- 每一份待签内容都由签名员在离线机上逐条核对后才签名；离线机保留只追加的本地签发日志。
- 演示密钥 `etos-demo-2026` 与 CI 临时密钥只用于演示与测试。正式版本（v* 标签）不包含演示公钥；`syapp-license-server keygen / issue` 拒绝正式密钥编号。

| 工具 | 运行位置 | 用途 |
|---|---|---|
| `syapp-license-signer` | 离线签名机 | 生成密钥（keygen）、核对与签名（inspect / sign）、核对结果（verify）、查看公钥（pubkey） |
| `syapp-license-server request / crl-request / import` | 厂商内网 | 生成签名请求、导入并核对签名结果、写出 `.lic` / `.crl` |
| `syapp-license-server serve`（授权后台 Web） | 厂商内网 | 以上在线侧步骤的界面化，加上客户、订单、审批、吊销与审计哈希链 |

`syapp-license-signer` 在发布页的 `edgetraffic-os-vendor-tools-<版本>-<系统>-<架构>` 包中（Linux / Windows / macOS），不包含在任何客户安装包里。程序不含网络代码（CI 用 `TestNoNetworkCode` 检查依赖）。
授权后台在 `edgetraffic-os-license-portal-<版本>-linux-<架构>` 包中，同样不交付客户。

## 2. 准备离线签名机（一次性）

1. 选一台专用电脑（笔记本即可），全盘加密，安装干净的操作系统。
2. 拔掉网线，在 BIOS / 系统中关闭无线与蓝牙。此后永不联网。
3. 在一台联网电脑上从发布页下载 `edgetraffic-os-vendor-tools-…` 与 `SHA256SUMS`，核对校验和，用一个**专用的传输 U 盘**拷入离线机。
4. 准备两个**备份 U 盘**（标记为 A、B），只用于存放加密私钥文件。
5. Linux 上签名工具会读取 `/sys/class/net` 检查网卡状态：检测到联网会警告；加 `-require-offline` 则直接拒绝运行，建议始终加上。

## 3. 密钥仪式（首次与每年轮换）

参与人：负责人与签名员两人（双人规则）。全程在离线机上完成，记录在纸质《密钥仪式记录》上并双方签字。

### 3.1 一次仪式生成四把密钥

| kid | 用途 | 平时 |
|---|---|---|
| `etos-prod-2027` | License 与吊销列表签名（主） | 日常签发 |
| `etos-prod-2027-b` | License 与吊销列表签名（备用） | 封存不用 |
| `etos-pkg-2027` | 数据面 / agent 升级包签名（主，与 License 密钥分开） | 每次发布升级包 |
| `etos-pkg-2027-b` | 升级包签名（备用） | 封存不用 |

**为什么要备用密钥**：控制台只信任随版本内置的公钥，节点只信任随 agent 内置的升级包公钥。只有主密钥时，主密钥一旦丢失或泄露，
新密钥签的 License / 升级包要等客户先把控制台、agent 升级到内置了新公钥的版本才能用，而这一步本身又要靠新密钥签的升级包，客户现场会断档。
备用密钥的公钥从第一个正式版本起就内置，出事时直接改用备用密钥签发，客户无需先升级。正式版本的发布门禁要求两类密钥都至少有两把
（`TestReleaseKeyRing`、`TestReleasePackageKeys`）。

```bash
# 双人规则：两人各输入自己的一半口令（每半至少 8 个字符），另一人回避；四把密钥各用不同的口令
for kid in etos-prod-2027 etos-prod-2027-b etos-pkg-2027 etos-pkg-2027-b; do
  ./syapp-license-signer keygen -kid $kid -out /media/backup-a -two-person -require-offline
done
```

每把密钥输出：

- `<kid>.key.json`：口令加密的私钥（Argon2id + AES-256-GCM；kid 与公钥受认证保护，改动即无法解密）。**明文私钥从不写入磁盘。**
- `<kid>.pub.txt`：kid、公钥、指纹，可以带出离线机。
- 屏幕上的公钥指纹（`SHA-256`，每 4 位一组）：两人分别抄写到仪式记录上并核对一致。

### 3.2 保管

1. 主密钥：`.key.json` 复制到备份 U 盘 A、B，分放两个保险柜；离线机本地保留一份（全盘加密）。
2. 备用密钥：复制到另外两个 U 盘 C、D，**与主密钥不放在同一个保险柜**，离线机上不保留。只在 3.3 的应急情况下取出。
3. 口令（双人规则时是两半口令）分别写在纸上密封，与 U 盘分开保管。**口令丢失则私钥不可恢复。**
4. 每半年按第 7 节抽查一次：取出一份备份，在离线机上 `syapp-license-signer pubkey -key …` 能解密并显示同一指纹（备份 U 盘会坏，口令信封会丢，抽查才能提前发现）。
5. 用传输 U 盘把四个 `.pub.txt` 带出，提交 PR：

   ```go
   // internal/license/keys.go
   var builtinKeys = map[string]string{
       "etos-prod-2027":   "<pub.txt 中的 public_key>",
       "etos-prod-2027-b": "<pub.txt 中的 public_key>",
   }
   // internal/pkgsig/keys.go
   var builtinKeys = map[string]string{
       "etos-pkg-2027":   "<pub.txt 中的 public_key>",
       "etos-pkg-2027-b": "<pub.txt 中的 public_key>",
   }
   ```

   审核人对照仪式记录逐把核对公钥指纹。该 PR 合并后，`v*` 正式版本的发布门禁才会通过。
6. 授权后台上线后，在后台“签名密钥”中登记 License 的两个 kid 与公钥（只填公钥）。

### 3.3 密钥丢失或泄露（应急）

先判断是哪一种：**丢失**是自己用不了了（U 盘损坏、口令信封丢失、离线机损坏），别人也拿不到；**泄露**是私钥文件连同口令可能落到他人手里
（只丢了加密文件而口令仍安全，按丢失处理，但要尽快完成轮换）。

| 情况 | 对已发出的 License / 已部署的节点 | 处置 |
|---|---|---|
| 主密钥丢了一份备份（另一份还能解密） | 无影响 | 从另一份再复制一份备份，补全两地保管；记录在案 |
| 主密钥全部备份或口令都丢失 | 已签发的 License 继续有效到期；节点照常运行 | 1. 改用备用密钥（`-b`）签发，客户无需升级；2. 尽快举行新仪式生成下一组主 / 备用密钥（如 `etos-prod-2027-c`），公钥随下一个控制台版本内置；3. 丢失的 kid 不必吊销（无人能用它签名），公钥保留到它签出的 License 全部到期 |
| 主密钥泄露 | 攻击者可以伪造 License / 升级包 | 1. 立即停止使用该 kid；2. 用备用密钥签发吊销列表，`revoked_kids` 中包含泄露的 kid（第 5 节），随更新后的 License 一起发给客户——客户控制台导入后即拒绝该 kid 签的所有 License；3. 用备用密钥为受影响客户重新签发 License；4. 把泄露的 kid 加入 `internal/license/keys.go` 的 `revokedKIDs`，并举行新仪式补一把新的备用密钥，发布控制台新版本 |
| 升级包主密钥泄露 | 攻击者可以伪造节点升级包（仍需能经控制台下发，控制台账号与 mTLS 是第二道防线） | 1. 立即停止使用该 kid；2. 用 `etos-pkg-…-b` 签发新版 agent / 数据面升级包，其中把泄露的 kid 加入 `internal/pkgsig/keys.go` 的 `revokedKIDs`；3. 通过控制台把全部节点升级到该版本，之后节点不再接受泄露 kid 签的包；4. 补一把新的备用密钥 |
| 主密钥与备用密钥同时不可用 | 已签发的 License 继续有效到期 | 只能举行新仪式、随新版本内置新公钥，客户须先在每台机器上手工安装新版本软件包（控制台与节点），才能导入新 License、接收新升级包。这是要用两地分开保管来避免的情况 |

注意：节点上的 agent 只校验 License 的签名与有效期，不读取吊销列表（第 5 节“当前限制”）；吊销在控制台侧生效。

### 3.4 年度轮换

1. 新年度前举行仪式生成下一年的四把密钥，公钥合入代码并随新版本发布（新旧公钥并存）。
2. 新版本发布、客户普遍升级后，在线侧开始用新 kid 签发；旧 kid 停止签发。
3. 旧 kid 的公钥保留在代码中，已签出的 License 继续有效直到到期；旧私钥按仪式记录销毁或封存。

## 4. 签发 License

推荐使用授权后台（4.0）；第 4.1–4.3 节的命令行流程与后台完全等价，可在后台不可用时使用。

### 4.0 授权后台

部署（厂商内网的一台 Linux 服务器，**不放任何私钥**）：

```bash
tar xzf edgetraffic-os-license-portal-<版本>-linux-amd64.tar.gz && cd edgetraffic-os-license-portal-*
install -m 0755 syapp-license-server /usr/bin/
useradd -r -s /sbin/nologin syapp-license
install -m 0644 syapp-license-server.service /etc/systemd/system/
mkdir -p /etc/syapp-license-server
# 可选：/etc/syapp-license-server/portal.env
#   ETOS_PORTAL_LISTEN=0.0.0.0:8600          默认只监听 127.0.0.1:8600
#   ETOS_PORTAL_DB=postgres://...            默认 SQLite（数据目录下 portal.db）
# 对内网开放时务必启用 HTTPS：在 ExecStart 追加 -tls-auto（内置 Web CA，浏览器导入 /web-ca.crt）
# 或 -tls-cert / -tls-key 使用公司证书
systemctl daemon-reload && systemctl enable --now syapp-license-server
```

首次打开页面创建管理员，再由管理员创建其他账号。角色：

| 角色 | 能做的事 |
|---|---|
| 销售 | 客户、订单；起草 License（粘贴客户的 `activation-request.json`，或容器部署只填安装 ID）、提交审批、续期 |
| 商务 | 审批 / 驳回；申请吊销 |
| 签名员 | 导出签名批次（下载 `sign-request-*.json`）、导入签名结果（`signed-*.json`）、取消批次 |
| 管理员 | 以上全部，以及用户管理、签名公钥登记 / 停用 / 作废 |

职责分离：同一张 License 的审批人不能是起草人或提交人（管理员也不例外）。

一张 License 的流转：**草稿 → 待审批 → 已审批 → 待离线签名 → 已签发**（可被驳回后重新提交；已签发的可续期、可吊销）。

1. 管理员在“签名公钥”登记离线签名机 keygen 输出的 kid 与公钥，两人核对指纹。后台只接受 `etos-prod-*` 编号的公钥，从不接收私钥。
   “未内置”标记表示该公钥还不在当前控制台版本的 `keys.go` 中：用它签发的 License 要等含此公钥的控制台版本发布后客户才能验证。
2. 签名员在“签名批次”导出批次：选择启用中的 kid，勾选已审批的 License，若有待发布的吊销可同时签发新吊销列表。浏览器下载 `sign-request-B0001.json`。
3. 离线签名（同 4.2）。
4. 签名员“导入签名结果”选择 `signed-B0001.json`。后台按 4.3 的规则逐条验签并与请求核对；通过后 License 变为“已签发”，新吊销列表发布。
5. 在 License 页“下载”得到 `.lic`（自动附上最新的吊销列表），发给客户导入。

吊销：商务在 License 页申请吊销（填写原因）→ 随下一个签名批次签发新的累积吊销列表 → 在“吊销”页下载最新吊销列表发给客户（或随下一次 `.lic` 一起下发）。
同一时间只能有一个包含吊销列表的未导入批次，保证吊销列表序号连续；批次签错或丢失可在“签名批次”取消，License 与吊销回到导出前状态。

所有写操作写入带哈希链的审计记录，“审计”页与总览会校验链是否完整。

### 4.1 在线侧：生成签名请求

客户在控制台“系统 → 授权”下载 `activation-request.json` 发给厂商。商务审批通过后：

```bash
syapp-license-server request -batch B0001 -kid etos-prod-2027 \
    -request activation-request.json -customer "某某公司" -customer-id C0001 \
    -edition professional -nodes 50 -sites 2000 -days 365 -out sign-request-B0001.json
# 同一批次可以多次执行追加（同一 -out）；容器部署用 -installation-id inst_... -binding installation_only
# OEM 定制品牌（docs/brand.md）：加 -brand syedge，License 只对该品牌的控制台有效；不加为通用
```

批次号按顺序递增（`B` 加 4–10 位数字）。用传输 U 盘把 `sign-request-B0001.json` 拷到离线机。

### 4.2 离线签名机：核对并签名

```bash
./syapp-license-signer inspect -in sign-request-B0001.json          # 先看清单
./syapp-license-signer sign -key etos-prod-2027.key.json -in sign-request-B0001.json \
    -log signer-log.jsonl -two-person -require-offline
```

- 工具逐条列出客户、版本、节点 / 站点数、生效与到期日、绑定方式、安装 ID，以及请求的 SHA-256。
- 签名员与商务审批单逐条核对，无误后**输入批次号**确认，再输入口令。
- 输出 `signed-B0001.json`（只含签名结果），并在 `signer-log.jsonl` 追加一行；同一份请求不会被签第二次。
- 签名后工具会用公钥把结果完整自检一遍。私钥只在内存中短暂存在，用完即清零。

把 `signed-B0001.json` 用传输 U 盘拷回在线侧。

### 4.3 在线侧：核对并导入

```bash
syapp-license-server import -request sign-request-B0001.json -signed signed-B0001.json -out licenses/
```

导入时逐条验签，并核对签名内容与请求中批准的 payload **完全一致**、条目一一对应、请求 SHA-256 一致；任何一项不符都拒绝。通过后写出 `licenses/<License 编号>.lic`，发给客户在控制台导入。

## 5. 吊销

吊销列表（CRL）是累积的：每份新列表包含之前全部吊销项，序号加 1。控制台只接受序号更大的列表（不能用旧列表“撤销吊销”），并拒绝列表中的 License 与 kid。

```bash
syapp-license-server crl-request -batch B0002 -kid etos-prod-2027 \
    -base crl-3.crl -revoke LIC-20261102-0002 -reason "合同终止" -out sign-request-B0002.json
# 离线签名同 4.2；导入同 4.3，写出 crl-4.crl
```

把吊销列表交给客户的方式：

- 附在客户收到的授权文件里（`.lic` 文件中每行一项，吊销列表是以 `etos-crl1.` 开头的一行），导入 License 时一并导入；
- 或在控制台“系统 → 授权”单独粘贴导入（接口 `POST /api/v1/license/crl`，授权过期 / 无效时仍可用）；
- 每次发布控制台新版本前，把最新吊销列表放入 `internal/license/builtin.crl`，随版本内置。

当前限制：吊销列表在控制台侧生效（控制台转为“授权无效”，停止新的变更）；节点上的 agent 只独立校验 License 的签名与有效期，不读取吊销列表。

## 6. 记录与审计

- 离线机：`signer-log.jsonl`（时间、批次、kid、请求 SHA-256、License 编号、CRL 序号），定期用传输 U 盘拷出归档。
- 在线侧：授权后台完整保存每个批次的请求与签名结果，并以审计哈希链记录每一步；使用命令行时自行归档每一份 `sign-request-*.json` 与 `signed-*.json`。
- 两边按请求 SHA-256 互相印证：每一条签发日志都应能在在线侧找到对应的请求与导入记录。
- 纸质：密钥仪式记录、口令信封的保管与开启记录。

## 7. 演练

正式启用前，用一个测试 kid（例如 `etos-prod-2099`，不合入代码）完整演练一遍：keygen → `request -kid etos-prod-2099` → sign → 在离线机上用 `syapp-license-signer verify -key … -request … -signed …` 核对（测试 kid 未内置，命令行 `import` 会按设计拒绝它）。
也可以用一套独立的演练授权后台（单独的数据目录）走完整流程：登记测试公钥 → 起草、审批 → 导出批次 → 离线签名 → 导入 → 吊销 → 再签发吊销列表。演练产生的 License 只用于核对流程，客户控制台不会接受（公钥未内置）。
确认人员熟悉流程后，销毁演练产生的私钥文件与演练后台的数据目录。
