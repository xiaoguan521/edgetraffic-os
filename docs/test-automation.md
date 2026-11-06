# 自动化验证入口与覆盖范围

本轮新增配置导入的真实 HTTP 集成测试、浏览器验收、双节点业务验证与 JUnit 报告。它们与已有 Go、demo、规模、国密、Lua、HA 测试配合使用；不表示 [完整清单](full-test-checklist.md) 的 208 项均已逐项自动化。

## 一键运行

仅使用脚本创建的临时测试控制台与测试密钥，不需要正式授权密钥：

```bash
# 浏览器依赖只安装到临时工具目录，不进入产品依赖。
npm install --no-save --no-audit --no-fund --prefix /tmp/etos-pw playwright@1.63.0
/tmp/etos-pw/node_modules/.bin/playwright install chromium

# SQLite + 真实浏览器 + 两个真实 syedge/agent 容器 + 测试后端。
NODE_PATH=/tmp/etos-pw/node_modules IMPORT_E2E_LIVE=1 scripts/ci/import-ui-local.sh
```

前提：Go、Node、pnpm、Python 3、Docker；能安装锁定的前端依赖，能拉取测试节点镜像。脚本先构建当前前端、控制台与 agent，然后创建独立临时环境；结束时清理进程、容器、网络及临时密钥。保留 `dist/test-reports/import-ui-local/junit.xml`，真实节点模式还保留节点镜像摘要记录。

可用 `IMPORT_NODE_IMAGE=ghcr.io/.../node@sha256:...` 固定数据面镜像；当前 agent 使用本地源码编译并注入临时测试公钥，不修改已有发布镜像。也可以通过 `IMPORT_TEST_DB` 指向独立的空 PostgreSQL 测试库，运行同一浏览器/双节点链路（该脚本不删除调用者提供的数据库）。控制台和 agent 默认端口为 28443/29443，节点 HTTP 映射为 28001/28002，可通过 `CONSOLE_PORT`、`GRPC_PORT`、`NODE1_PORT`、`NODE2_PORT` 修改。测试容器使用独立网络，默认子网 `172.29.231.0/24`，可用 `IMPORT_TEST_SUBNET` 更换为空闲且在默认允许回源网段内的子网。

```bash
# 不使用 Docker 的控制台/浏览器模式；双节点业务用例明确记为 skipped。
NODE_PATH=/tmp/etos-pw/node_modules IMPORT_E2E_LIVE=0 scripts/ci/import-ui-local.sh

# 后端 HTTP 集成测试：默认每个用例独立 SQLite 数据库，含 race。
make test-imports

# PostgreSQL：每个用例创建并清理独立 schema；账号须允许创建 schema。
ETOS_TEST_DB='postgres://user:password@127.0.0.1:5432/testdb?sslmode=disable' make test-imports
```

连接已经初始化的独立 demo 环境时，可直接运行以下命令。该环境必须使用测试密钥、有效专业版授权、关闭发布审批；用例会新建用户、组、导入记录和业务草稿，并临时切换过期授权。`finally` 中恢复有效测试授权。不能将其当作只读探测脚本运行在生产环境。

```bash
BASE=http://127.0.0.1:8443 \
LIC_TOOL=/tmp/lic LIC_KEY=/tmp/keys/etos-ci-ephemeral.key LIC_KID=etos-ci-ephemeral \
NODE_PATH=/tmp/etos-pw/node_modules IMPORT_E2E_LIVE=1 \
make test-import-ui
```

非默认拓扑用 `IMPORT_GROUP_ID`、`IMPORT_UPSTREAM`、`IMPORT_NODE_URLS` 指定。`IMPORT_E2E_LIVE=1` 会要求至少两台在线节点；真实请求失败或节点未生效会让测试失败，不允许把零节点发布当成通过。

## 新增用例与清单映射

以下是相关清单编号的子场景覆盖，不代表对应编号下的全部输入组合均自动覆盖。

| 自动化入口 | 覆盖的场景 | 清单编号 |
| --- | --- | --- |
| `TestImportHTTPDraftWorkflow` | 上传不创建草稿、下载报告、确认后草稿、片段自批被拒、另一人批准后进入渲染、重复应用幂等保护 | IMP-01/04/05/06，SNP-02 |
| `TestImportHTTPAccessMatrix` | 四角色及匿名对导入查询/上传/应用的服务端门禁，CSRF 拒绝 | IMP-08，AUTH-01/08 |
| `TestImportHTTPReadOnly` | 过期禁止上传和应用，报告可读，恢复后可应用 | IMP-08，LIC-07 |
| `TestImportHTTPInvalidUploads` | 缺少文件、语法错误、破损 gzip、超 20 MiB、归档路径穿越；拒绝后不写记录或草稿 | IMP-07 |
| `TestImportHTTPConcurrentApply` | 并发应用同一记录只有一次成功，另一请求 409，只有一个草稿 | IMP-05 |
| `TestImportHTTPArchiveSecrets` | 含 HTTPS 证书的归档、报告/响应无私钥、应用后加密入库且可解密 | IMP-02，CERT-04 |
| `TestImportHTTPGroupAndQuota` | 历史记录按组过滤，批量导入不能绕过站点配额 | IMP-03/08，LIC-03 |
| `TestImportHTTPBlockingReport` | 含不支持指令的站点部分转换，应用后停用 | IMP-04/06 |
| `import-acceptance.cjs` 的 12 个浏览器/业务场景 | 登录返回目标路由、直接刷新、文件选择上传、逐行报告、浏览器下载、应用结果、重复应用、权限 UI+API、片段审批、切组、过期置灰/恢复、资源无 404 | UI-01/02，IMP-01/04/05/06/08，AUTH-01，SNP-02，LIC-07 |
| 浏览器套件的真实节点场景 | 发布导入草稿，等待所有目标节点 APPLIED；两节点按导入 Host 实际回源；停用站点进入默认拒绝行为 | MVP-10，IMP-05 |
| `TestDriftReportWithSymlinkedRoot` | 配置根路径经符号链接时仍发现变更、删除和新增私钥；目录外仍不读取 | DRF-01/02 |

HTTP 集成测试直接使用生产路由、中间件和数据库，没有测试专用的权限替代实现。手动执行 `make test` 或手动触发 `ci` 时，会分别运行这些测试；PostgreSQL 任务会再运行同组测试。

## 手动触发与报告

`ci`、`demo-e2e`、`lua`、`packages`、`scale` 五个验证工作流均仅保留 `workflow_dispatch`，提交代码或创建 PR 不会触发这些工作流。在 GitHub 仓库 Actions 中选择工作流，再点击 **Run workflow** 才会运行。

手动触发 `.github/workflows/demo-e2e.yml` 后，在 SQLite/PostgreSQL 两种数据库环境中，先执行已有真实 demo e2e，再运行新增浏览器套件。原有浏览器权限套件仍保留于 SQLite 任务。

新增浏览器套件使用 Node 原生测试运行器，控制台输出逐场景结果，写入 JUnit XML。无论成功失败，工作流均上传 `import-acceptance-sqlite` / `import-acceptance-postgres` 报告，保留 14 天。未启动或前置条件失败不能计为通过；控制台模式的业务跳过项会明确写进报告。

### 本地推送前检查（替代自动 CI）

工作流改为手动触发后，合并前的回归由本地 `pre-push` 钩子兜底。每个克隆执行一次：

```bash
make hooks   # git config core.hooksPath scripts/git-hooks
```

之后每次 `git push` 会先执行 `make test`（`go vet` + `go test -race`）与 `make brand`，失败则拒绝推送；只改动 `docs/` 或 `*.md` 时跳过。

| 变量 | 作用 |
| --- | --- |
| `ETOS_PREPUSH_FULL=1` | 额外执行 `make gen-check`、`make lint`（已安装 golangci-lint 时）与前端 `pnpm build`（已安装依赖时） |
| `ETOS_SKIP_PREPUSH=1` | 跳过本次检查（等同 `git push --no-verify`），仅限紧急情况，事后补跑 |

本地钩子只覆盖单元与 HTTP 集成测试。PostgreSQL、数据面黄金用例、国密、Lua、打包与规模验收依赖专用环境，**打版本标签前必须手动触发一次完整的 `ci` 与 `demo-e2e`**，并把结果记入 `docs/test-runs/`。

现有长周期/专用环境测试继续从 [量化验收对照](acceptance.md) 进入。正式密钥仪式、国产 OS 实机和 10 份客户配置验收仍需要真实材料及现场环境，不能由合成样本代替。

## 2026-11-02 本地实测

源码为 `2b54940` 加本轮工作区修改；执行环境为 macOS arm64、Go 1.26.4、Node 24.20.0、Docker/OrbStack，PostgreSQL 16.4。节点数据面使用 `node@sha256:bb134a5300a6d2bea1793558a45fc6f5e08e16161369100583c6009f5fee3289`，agent 为本地当前源码构建。

| 检查 | 结果 |
| --- | --- |
| `make test` | 全仓 vet + Go race 测试通过；修复并新增了符号链接配置根目录的漂移回归用例 |
| `make test-imports` | SQLite 的 8 组 HTTP 集成测试通过 |
| PostgreSQL `make test-imports` | PostgreSQL 独立 schema 的同组 HTTP race 测试通过 |
| SQLite 浏览器 + 双节点 | 12 个场景通过，0 失败、0 跳过；真实发布后两节点均回源成功 |
| PostgreSQL 浏览器 + 双节点 | 同样 12 个场景通过，0 失败、0 跳过 |
| 控制台浏览器模式 | 11 个场景通过，真实节点场景明确跳过；不作为双节点通过证据 |
| 前端构建、`make brand`、`make gen-check`、`git diff --check` | 通过 |
| 新增改动 lint | `golangci-lint run --new-from-rev=HEAD ./...` 通过 |
| 全仓 `make lint` | 未通过：golangci-lint 2.15.0 报告 60 个现有问题（48 errcheck、1 ineffassign、11 staticcheck）；不计为本轮通过，也未为此提交代码 |
| GitHub CI | 已配置双数据库执行及报告上传；本轮尚未推送工作区修改或运行远端工作流 |

报告在 `dist/test-reports/import-ui-live/junit.xml`（SQLite）与 `dist/test-reports/import-ui-postgres/junit.xml`（PostgreSQL），检查日志保留于 `dist/test-reports/validation/`。这些是本轮新增范围的实测结果，不是 208 项全量清单的通过记录。
