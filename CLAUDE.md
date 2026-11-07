# SyEdge —— 编码规则（违反任一条即拒绝合并）

完整设计见 docs/spec/README.md 中的规格书链接。每个任务只引用规格书对应章节。

## 技术栈
- 后端 Go + Gin + GORM/gen + gormigrate + grpc；前端 Vue 3 + TS + Vite + antdv-next + Pinia。
- 不引入 Redis / MinIO / etcd / Vault / SkyWalking。新增依赖必须是 MIT / BSD / Apache-2.0 许可。
- 不得复制 nginx-ui（AGPL-3.0）的任何代码。

## 数据面
- syedge = nginx 官方 stable 源码 + dataplane/patches/0001-syedge-rename.patch。
  patch 只允许改字符串字面量与宏的值，禁止改动判断、结构体、函数签名、模块 ABI。
- 信号：reload=HUP，quit=QUIT，stop=TERM，reopen=USR1；USR2 与 WINCH 仅用于二进制热升级流程
  （USR2 发给当前 master 启动新版，WINCH 只发给旧 master `syedge.pid.oldbin` 让旧 worker 退出），均由 wrapper 发出。
- Lua 发行变体（P10）：lua-nginx-module、ngx_devel_kit、LuaJIT 以动态模块形式放在 /opt/syedge/modules/，
  不改 syedge 核心与改名 patch。只有该目录下的 Lua 模块文件免于“二进制无上游名”检查；进程名、命令、日志、包名仍须为 syedge。
- 国密发行变体（P8）：`build-syedge.sh -t tongsuo` 只把 OpenSSL 换成铜锁（开启 NTLS），不改 syedge 源码与改名 patch；
  国密双证书只通过 syedge 已有的 `ssl_conf_command`（Enable_ntls / SignCertificate / EncCertificate …）由 internal/render 生成，
  路径写本版本目录的绝对路径（见 docs/gm.md）。
- 不开放客户自定义 Lua 脚本：Lua 代码只能由 internal/render 中审核过的模板生成，参数经校验与转义。
- 永远不得 restart syedge；任何代码路径（含授权失效）不得停止 syedge。
- 运行时不使用 root（设计 0001 D5）：master、worker 与 wrapper 都以 syedge 运行，只带 CAP_NET_BIND_SERVICE
  （单元的 AmbientCapabilities；wrapper 为文件能力，只允许 syedge 组执行）；防火墙的开机加载与刷新以 syedge-firewall 运行，
  只带 CAP_NET_ADMIN。sudoers 以 (syedge) 运行 wrapper，wrapper 拒绝以 root 运行。已以 root 运行的 master 由
  syedge-instance nonroot 经热升级机制无中断切换，不得用 restart。只有安装、卸载与 syedge-instance 等管理命令以 root 执行。
  唯一例外是兼容模式（内核或 systemd 不支持 ambient 能力，如 CentOS 7；或 --root-master 显式选择）：与 nginx 相同，
  master 与 wrapper 以 root 运行、worker 仍以 syedge 运行，sudoers 为 (root)；模式只由 root 所有的 /etc/syedge/root-master 决定。

## 配置
- 禁止用字符串拼接用户输入生成配置；一律经 internal/render 的校验器 + 模板 + 转义函数。
- 版本目录自包含，include 一律相对路径；--conf-path=/opt/syedge/conf/current/syedge.conf。
- location 内禁止 add_header；location 内出现 proxy_set_header 时必须重发基线 5 条。
- 每次修改模板都要让 render 黄金用例通过真实 `syedge -t`。

## agent 与安全
- agent 只主动连接控制台，不得监听任何端口。
- agent 协议中不得加入「执行命令」「读写任意文件」类指令。仅有两个受限的专用消息：
  - 漂移 diff 上报：只限受管版本目录 /opt/syedge/conf/releases/ 内的文件，单文件大小设上限，私钥文件只报哈希。
  - UpgradeDataplane：升级包必须带厂商 Ed25519 签名（与 License 签名密钥分开），由 wrapper 验签后才可使用。
- 所有外部命令用 exec.Command 切片调用，禁止 sh -c。
- wrapper 只接受子命令 test|reload|reopen|quit|upgrade-start|upgrade-commit|upgrade-abort（升级三个在 P8 实现）；
  test 只接受 ^r[0-9]{6,12}$，upgrade-start 只接受 ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$；每个子命令都必须有注入测试；
  sudoers 每个子命令单独一行（共 7 行，以 (syedge) 身份运行），只有 test 与 upgrade-start 带参数通配，参数安全由 wrapper 校验。
- 私钥、凭据、License 签名私钥不得出现在代码、日志、前端响应中。
- 数据库访问只用参数化查询；MVP 阶段集中在 internal/console/store 封装，正式版迁移到 gorm/gen。每个写接口写审计。

## 授权
- 业务代码只通过 license.Evaluate 产出的 Snapshot 读取授权，不得自行解析 .lic。
- 需授权的 API 必须挂 RequireFeature / RequireQuota / RequireWritable 中间件；前端置灰不算约束。
- EXPIRED/INVALID 状态下回滚、证书续期、导入 License、导入吊销列表必须仍可用。
- 正式 License 与吊销列表只由离线签名机上的 syedge-license-signer 签名（规格书 8.8.1，docs/license-ops.md）：
  该程序不得引入任何网络代码（TestNoNetworkCode）；私钥只以口令加密文件存在；在线侧（授权后台）只持有公钥。
  syedge-license-server 的明文私钥命令（keygen / issue）只允许 etos-demo-* / etos-ci-*；v* 正式版本用 -tags release 构建，不含演示公钥。

## 品牌
- 产品代码、界面文案、日志、打包脚本中不得出现 "nginx"（范围见 scripts/brand-check.sh；/opt/syedge/modules/ 下的 Lua 模块是唯一例外，见“数据面”）。
- 关于页与 docs/NOTICE 必须保留 "Based on NGINX Open Source (BSD-2-Clause)"。

## 依赖
- 依赖全部 vendor 在仓库中：新增依赖时在 internal/deps/tools.go 引用并推送到 deps/** 分支，由 deps 工作流 tidy + vendor 后提交。
- 前端 node_modules 缓存在 deps-cache/web 分支（xz 分片），离线构建时解压到 web/。

## 交付要求
- 先写失败用例再写实现；每个 PR 附可运行的测试命令。
- make test、make lint、make brand 全部通过才可提交。
