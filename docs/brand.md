# 定制品牌构建（OEM）

设计见 `docs/design/0001-deploy-layout.md` 第 4 节。

**产品默认品牌**（2026-11-06 起，此前为 syapp / EdgeTraffic OS）：命令与路径 `syedge`，显示名 `SyEdge`，
产品名 `SyEdge`，中文全称“神玥企业级动态网关与负载均衡引擎”（`brand.env`）。下文是为其他品牌（OEM）构建的方法，示例用测试品牌 `sytest`。
品牌在**构建时**确定：进程标题、错误日志前缀、`Server` 响应头、错误页都编译在数据面二进制中，安装后只改文件名无法改干净。一个品牌对应一套安装包与升级包。

## 1. 品牌参数

复制仓库根目录的 `brand.env` 并修改，不要直接改它（默认值即当前产品）：

```bash
cp brand.env /path/to/brand-sytest.env
# BRAND_ID=sytest             命令名、二进制、路径、用户、服务单元、安装包名（小写字母开头，3–7 位小写字母或数字）
# BRAND_PROC=SyTest           ps 中的 "SyTest: master process"、日志前缀、-v、Server 头、错误页
# BRAND_PRODUCT="SyTest"      控制台与关于页显示的产品名
# BRAND_FULLNAME="……"         产品中文全称（登录页、关于页；不设时同 BRAND_PRODUCT）
export BRAND_ENV=/path/to/brand-sytest.env
```

## 2. 构建

```bash
dataplane/build-syedge.sh -S -o dist/dp                    # 数据面：从模板生成改名 patch，产物 dist/dp/sytest-<版本>-<架构>.tar.gz
scripts/release/package.sh node amd64 dist/dp/sytest-*.tar.gz   # 节点包 sytest-node-...tar.gz
scripts/release/package.sh pkg amd64 dist/dp/sytest-*.tar.gz    # RPM / DEB：sytest、sytest-agent、sytest-console
scripts/release/package.sh console linux amd64              # 控制台
```

各脚本在定制品牌下都会检查产物：文件名与文本中不得残留默认品牌名，二进制中只允许 Go 模块路径与协议 / 数据库字段名（见下文“已知限制”），否则构建失败。前端只构建一次，界面上的名字在运行时从 `/api/v1/brand` 读取。

## 3. 本地自检

```bash
make brand-matrix          # 用测试品牌 sytest / SyTest 完整检查：patch 生成、全部 Go 程序、节点发布包
```

构建出数据面后，用真实二进制检查对外可见的名字（需要 root）：

```bash
sudo scripts/ci/brand-runtime.sh dist/syedge-1.30.6-x86_64.tar.gz syedge SyEdge
```

它检查 `-v` / `-t` 输出、master 与 worker 进程标题、HTTP/1.1 与 HTTP/2 的 `Server` 头（应为显示名：默认品牌为 `SyEdge`）、默认错误页、`$<品牌>_version` 变量，以及二进制中没有上游名和默认品牌名。HTTP/2 的 `Server` 头是 HPACK 哈夫曼编码，`strings` 扫描看不到，必须这样实测。

默认品牌的产物与引入品牌参数前完全相同（`TestDefaultBrandMatchesCommittedPatch`：默认品牌生成的 patch 与 `dataplane/patches/0001-syedge-rename.patch` 逐字节相同）。

## 4. 已知限制

- **Go 程序的内部标识**：模块路径 `github.com/xiaoguan521/edgetraffic-os/...`（GitHub 仓库名）、agent 协议的字段名 `syapp_version_output` / `syapp_sha256`、数据库与接口字段名 `syapp_version` / `syapp_sha256`（更名前的历史名，迁移已冻结、协议需与已部署的 agent 兼容）保留不变。它们只在 `strings` 或崩溃堆栈中可见，不出现在进程名、命令、路径、界面、日志与响应中；修改它们需要重命名 Go 模块、协议字段与数据库列，收益很小。
- **授权与品牌**：授权文件暂不绑定品牌（设计待决策 D3）。
- **注册时的品牌核对**：不同品牌的 agent 与控制台之间，升级包因签名包名不同而互不接受；注册时还不校验品牌（需要新增协议字段）。
- 已安装的节点不能原地改品牌：新装一套后用配置导入（P7）迁移。
