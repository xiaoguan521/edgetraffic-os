# 从源码编译

本页是开发、移植或制作发行包时的源码编译步骤。一般节点安装直接使用发布包，见 [安装维护手册](install-maintenance.md) 第 4 节。


源码编译会生成可安装的压缩包，不建议直接把上游源码目录中的二进制复制到生产机。数据面构建脚本会校验上游版本和提交哈希、应用唯一改名 patch，并执行品牌泄漏检查。

## 安装编译依赖

Debian/Ubuntu：

```bash
sudo apt-get update
sudo apt-get install -y \
  build-essential ca-certificates curl git make patch perl \
  gzip bzip2 xz-utils tar binutils
```

RHEL/CentOS/Rocky/openEuler 类系统：

```bash
sudo dnf install -y \
  gcc gcc-c++ make ca-certificates curl git patch perl \
  gzip bzip2 xz tar binutils
```

不使用 `-S` 的普通编译还需要系统开发库：

```bash
# Debian/Ubuntu
sudo apt-get install -y libpcre2-dev libssl-dev zlib1g-dev
```

## 编译标准版 syedge

在仓库根目录执行：

```bash
cd /path/to/edgetraffic-os

# 可选：先编译 Go 组件并执行测试
make test
make web       # 构建控制台前端，需要 pnpm
make build     # 生成 bin/syedge-console、bin/syedge-agent、bin/syedge-wrapper 等

# 推荐：依赖静态链接，产物适合多种 glibc 发行版
dataplane/build-syedge.sh -S -t openssl -o dist/syedge-standard
```

产物类似：

```text
dist/syedge-standard/syedge-1.30.6-x86_64.tar.gz
```

`-S` 会根据 `dataplane/DEPS` 下载并校验 OpenSSL、PCRE2 和 zlib 源码，并把它们静态链接到 `syedge` 中。未设置 `SYEDGE_PREFIX` 时，构建产物解压到 `/` 后包含 `/opt/syedge` 目录树；生产 Action 默认将该变量设为 `/home/atwasoft/syedge`，也可在手动触发时通过 `prefix` 入参调整。完整的节点安装还需要 Agent、wrapper 和 systemd 单元，生产环境应继续制作节点发布包。

如果必须使用系统库，可以省略 `-S`：

```bash
dataplane/build-syedge.sh -t openssl -o dist/syedge-standard
```

## 编译国密版 syedge

国密版使用铜锁（Tongsuo）开启 NTLS。国密是构建时选择的 TLS 变体，不是启动参数，也不是只在配置中增加 `ssl_conf_command` 就能实现。

```bash
cd /path/to/edgetraffic-os
dataplane/build-syedge.sh -S -t tongsuo -o dist/syedge-gmssl
```

脚本会：

- 按 `dataplane/UPSTREAM` 校验铜锁版本和提交哈希；
- 配置 `--build=gmssl` 和 `enable-ntls`；
- 静态链接铜锁、PCRE2、zlib；
- 生成类似 `syedge-1.30.6-gmssl-x86_64.tar.gz` 的国密数据面包。

编译后立即检查：

```bash
tar -C /tmp -xzf dist/syedge-gmssl/syedge-1.30.6-gmssl-x86_64.tar.gz
/tmp/opt/syedge/sbin/syedge -v
/tmp/opt/syedge/sbin/syedge -V | grep enable-ntls
```

预期 `-v` 中包含 `(gmssl)`，`-V` 的 configure 参数中包含 `enable-ntls`。如果没有这两项，不要把该包当作国密包发布。

## 在兼容性构建环境中编译

为了让同一个数据面包覆盖 CentOS 7、麒麟 V10、统信 UOS、openEuler 以及较新的发行版，建议使用项目提供的 manylinux2014 构建脚本。该脚本按本机架构运行 Docker：

```bash
# 标准版
scripts/release/build-syedge-static.sh dist/syedge

# 国密版
TLS_LIB=tongsuo scripts/release/build-syedge-static.sh dist/syedge-gmssl
```

如果同时制作 Lua 附加包：

```bash
LUA=1 scripts/release/build-syedge-static.sh dist/syedge-lua
```

该命令会在 `dist/syedge-lua/base/` 中同时输出标准版和国密版基础包，并在 `dist/syedge-lua/` 输出一个同时适配两种基础包的 Lua 附加包。

## 编译 Lua 附加包

Lua 不是把任意脚本直接静态塞进 `syedge`，而是作为版本绑定的动态模块附加包构建。推荐使用统一脚本：

```bash
cd /path/to/edgetraffic-os
dataplane/build-syedge-lua.sh -o dist/syedge-lua
```

脚本会按 `dataplane/lua/DEPS` 锁定并校验以下组件：LuaJIT、ngx_devel_kit、OpenResty Lua 模块、lua-resty-core 和 lua-resty-lrucache。输出包括：

```text
dist/syedge-lua/syedge-lua-1.30.6-x86_64.tar.gz
dist/syedge-lua/base/syedge-1.30.6-x86_64.tar.gz
dist/syedge-lua/base/syedge-1.30.6-gmssl-x86_64.tar.gz
```

不要只用一套 `--add-module` 参数手工编译后把模块复制到任意版本的 `syedge`。Lua 动态模块与 `syedge` 的版本、导出符号和 TLS 变体绑定，统一脚本会同时编译标准模块和国密模块并执行符号自检。

## 组装节点发布包

使用已生成的数据面包制作完整节点包：

```bash
VERSION=0.1.0 \
  scripts/release/package.sh node amd64 \
  dist/syedge/syedge-1.30.6-x86_64.tar.gz
```

如果需要 RPM/DEB：

```bash
VERSION=0.1.0 \
  scripts/release/package.sh pkg amd64 \
  dist/syedge/syedge-1.30.6-x86_64.tar.gz
```

控制台发布包需要先构建前端：

```bash
make web
VERSION=0.1.0 scripts/release/package.sh console linux amd64
```

