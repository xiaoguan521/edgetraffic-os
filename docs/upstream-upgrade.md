# 上游版本跟踪与升级操作手册

本文面向开发团队，描述当 nginx 官方发布新 stable 版本时，如何将 SyEdge 数据面同步升级。
客户侧的升级流程（RPM/DEB 包升级、控制台热升级）见 `install-maintenance.md` 第 9 节。

## 1. 版本锁定机制

项目通过以下文件锁定所有上游依赖的精确版本：

| 文件 | 内容 |
|---|---|
| `dataplane/UPSTREAM` | nginx 源码的 tag、commit hash、版本号；铜锁的 tag 与 commit |
| `dataplane/DEPS` | 静态链接依赖（OpenSSL、PCRE2、zlib）的版本、下载地址、sha256 |
| `dataplane/patches/0001-syedge-rename.patch` | 唯一的改名 patch，基于当前锁定的上游版本 |

`build-syedge.sh` 在构建时会：
1. 按 `UPSTREAM` 中的 tag 拉取源码，校验 commit hash 一致；
2. 校验源码中的 `NGINX_VERSION` 与 `UPSTREAM_VERSION` 一致；
3. 应用改名 patch，运行 `rename-scan.sh` 检查残留；
4. 按 `DEPS` 下载依赖，校验 sha256；
5. 构建后自检：版本输出、二进制无上游名称泄露、静态链接只依赖 glibc。

任何一步不通过，构建即失败。

## 2. 安全监控：何时需要升级

### 2.1 需要关注的信息源

| 来源 | 地址 | 关注内容 |
|---|---|---|
| nginx 官方安全公告 | https://nginx.org/en/security_advisories.html | 所有 CVE，尤其影响 stable 分支的 |
| nginx 发布日志 | https://nginx.org/ | stable 分支新版本 |
| OpenSSL 安全公告 | https://openssl.org/news/vulnerabilities.html | 影响 3.x 的 CVE |
| PCRE2 发布 | https://github.com/PCRE2Project/pcre2/releases | 安全修复 |
| 铜锁发布 | https://github.com/Tongsuo-Project/Tongsuo/releases | 国密变体依赖 |

### 2.2 升级触发条件

**必须升级（安全）：**
- nginx stable 发布了包含安全修复的新版本。
- OpenSSL 发布了影响当前使用大版本的安全修复。

**建议升级（稳定性）：**
- nginx stable 发布了 bugfix 版本。
- PCRE2 或 zlib 发布了安全或重要 bugfix 版本。

**可选升级（功能）：**
- nginx 发布了新的 stable 大版本（如 1.30 → 1.32），带有产品需要的新功能。
  大版本升级需评估 patch 兼容性和功能变化，应在独立分支上完成。

### 2.3 自动检查脚本

`scripts/ci/check-upstream.sh` 检查 `dataplane/UPSTREAM` 与 `dataplane/DEPS` 锁定的版本在**同一版本线**内是否有新版本：
上游 stable（如 1.30.x）、OpenSSL（如 3.5.x）、PCRE2、zlib、铜锁（如 8.x）。有新版本时退出码为 1，查询失败为 2；
发现更新的 stable 版本线（如 1.32）时只提示，不算失败（大版本升级见第 4 节）。

```bash
scripts/ci/check-upstream.sh
```

CI 中由 `.github/workflows/upstream-check.yml` 每周一运行一次（也可在 Actions 页手动触发），有新版本时该工作流失败并给出注解。

## 3. 升级操作步骤

### 3.1 前置准备

```bash
# 从 main 创建升级分支
git checkout main && git pull
git checkout -b upgrade/nginx-<新版本>
```

### 3.2 更新 nginx 版本

```bash
# 1. 查询最新 stable tag 与 commit
git ls-remote --tags https://github.com/nginx/nginx.git 'release-1.30.*' | sort -V -t/ -k3
# 记下最新 tag（如 release-1.30.7）

# 2. 获取该 tag 的 commit hash
git ls-remote https://github.com/nginx/nginx.git refs/tags/release-1.30.7
# 输出示例：a1b2c3d4...  refs/tags/release-1.30.7

# 3. 编辑 dataplane/UPSTREAM
# 更新以下三行：
#   UPSTREAM_VERSION=1.30.7
#   UPSTREAM_TAG=release-1.30.7
#   UPSTREAM_COMMIT=<步骤 2 获得的完整 hash>
```

### 3.3 重新应用改名 patch

`build-syedge.sh` 会自动在新版源码上执行 `patch -p1`。大多数小版本升级 patch 可以直接应用。

**改名 patch 的来源是模板。** 仓库中的 `0001-syedge-rename.patch` 由模板 `dataplane/patches/0001-rename.patch.tmpl`
经 `dataplane/gen-rename-patch` 生成，两者必须逐字节一致（`TestDefaultBrandMatchesCommittedPatch`），OEM 品牌也从同一个模板生成。
因此 patch 冲突时**改模板**，不要直接改生成的 patch：

```bash
# 1. 在新版本源码上试应用，找出冲突的文件
WORK=$(mktemp -d)
git clone --depth 1 --branch release-1.30.7 https://github.com/nginx/nginx.git "$WORK/src"
(cd "$WORK/src" && patch -p1 --dry-run < "$OLDPWD/dataplane/patches/0001-syedge-rename.patch")

# 2. 修改模板中对应的 hunk（上下文行与行号），占位符 @ID@ / @IDUP@ / @PROC@ / @PROCLEN2@ / @HPACK@ 保持不变；
#    修复原则（ADR 0001）：只改字符串字面量与宏的值，不改判断、结构体、函数签名、模块 ABI；
#    对照 dataplane/patches/README.md 的文件表逐一检查

# 3. 由模板重新生成默认品牌的 patch，并跑生成器测试与其他品牌的矩阵
go run ./dataplane/gen-rename-patch -id syedge -proc SyEdge -o dataplane/patches/0001-syedge-rename.patch
go test ./dataplane/gen-rename-patch/
make brand-matrix
```

修复后同步更新 `dataplane/patches/README.md` 中的版本号和文件表。

**HTTP/2 server 头：** 模板中的 `@HPACK@` 由 `gen-rename-patch` 按品牌自动计算，无需手工处理；
`dataplane/hpack-gen` 是早期只支持小写字母的小工具，编码表不含大写字母，不能用于 `SyEdge`，仅保留作历史参考；以 `gen-rename-patch` 及其测试为准。

### 3.4 更新静态依赖（按需）

如果 OpenSSL、PCRE2 或 zlib 也需要升级，编辑 `dataplane/DEPS`：

```bash
# 以 OpenSSL 为例
# 1. 从 https://github.com/openssl/openssl/releases 找到新版本
# 2. 下载 tar.gz 并计算 sha256
curl -fsSL -o /tmp/openssl-3.5.10.tar.gz \
  https://github.com/openssl/openssl/releases/download/openssl-3.5.10/openssl-3.5.10.tar.gz
sha256sum /tmp/openssl-3.5.10.tar.gz

# 3. 更新 DEPS 中的 OPENSSL_VERSION、OPENSSL_URL、OPENSSL_SHA256
```

铜锁（国密变体）升级同理，编辑 `dataplane/UPSTREAM` 中的 `TONGSUO_*` 字段。

### 3.5 构建与验证

```bash
# 标准版构建（静态链接）
dataplane/build-syedge.sh -S -o dist/

# 国密变体构建
dataplane/build-syedge.sh -S -t tongsuo -o dist/

# Lua 附加包构建（如适用；同时产出标准版与国密版基础包到 dist-lua/base，-v 可指定附加包版本）
dataplane/build-syedge-lua.sh -o dist-lua/

# 项目测试
make test
make lint
make brand
```

构建脚本内置的自检会验证：
- `syedge -v` 输出正确的版本和品牌名
- 二进制中无上游名称泄露
- 国密版包含 `enable-ntls` 和 `(gmssl)` 标识
- 静态构建只依赖 glibc
- Lua 模块符号全部可解析

### 3.6 回归测试要点

除了 `make test` 覆盖的自动化用例，升级后应重点关注：

1. **配置渲染**：用 `syedge -t` 验证现有黄金用例全部通过。
2. **热升级**：在测试环境从旧版本热升级到新版本，确认连接不中断。
3. **HTTP/2 和 HTTP/3**：验证 server 头正确返回品牌名。
4. **国密握手**（如适用）：用铜锁 CLI 做 NTLS 握手测试。
5. **Lua 模块加载**（如适用）：确认 `load_module` 成功且功能正常。

### 3.7 提交与发布

```bash
# 提交变更
git add dataplane/UPSTREAM dataplane/DEPS dataplane/patches/
git commit -m "upgrade: nginx $OLD_VERSION -> $NEW_VERSION

修复 CVE-xxxx-xxxxx, CVE-xxxx-xxxxx（列出本次修复的 CVE）。
上游发布说明：https://nginx.org/en/CHANGES-1.30"

# 创建 PR，附上：
# - 上游 changelog 链接
# - 修复的 CVE 列表
# - 构建日志截图或 CI 链接
# - 回归测试结果
```

## 4. 大版本升级（如 1.30 → 1.32）

大版本升级除了上述所有步骤外，还需要额外评估：

1. **废弃指令**：检查上游 CHANGES 中标记为 deprecated 或 removed 的指令，
   确认 `internal/render` 模板中没有使用它们。
2. **新增指令**：评估是否需要在控制台中暴露新功能。
3. **模块 ABI 变化**：如果 ABI 版本号变了，Lua 动态模块必须重新编译。
4. **变量名变化**：确认 `$syedge_version` 等自定义变量不受影响。
5. **默认值变化**：检查上游是否改变了默认配置值（如 keepalive、buffer 大小）。

建议在独立分支上完成，经过完整的验收测试（`docs/full-test-checklist.md`）后再合入。

## 5. 历次升级记录

| 日期 | nginx 版本 | 主要原因 | PR |
|---|---|---|---|
| 2026-10-14 | 1.30.6 | 初始锁定版本（修复的 CVE 以上游安全公告为准：https://nginx.org/en/security_advisories.html） | 初始版本 |
| 2026-11-25 | 1.30.8 | 修复 CVE-2026-132779 | — |
