#!/usr/bin/env bash
# 发布打包：交叉编译 Go 程序并生成可直接下载使用的发布包。
#
#   scripts/release/package.sh console <goos> <goarch>
#       控制台包：syedge-console（前端已嵌入，须先构建 web/dist）；开发构建另带演示用的 syedge-license-server
#       产物 syedge-console-<ver>-<goos>-<goarch>.tar.gz（windows 为 .zip）
#
#   scripts/release/package.sh node <goarch> <syedge tarball>
#       节点包（仅 Linux）：syedge 数据面 + syedge-agent + syedge-wrapper + systemd 单元 + install.sh
#       产物 syedge-node-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh all <goarch> <节点包> <国密基础包> <Lua 附加包>
#       整合包：节点包 + extras/gmssl（国密版二进制）+ extras/lua（Lua 附加包），产物 syedge-all-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh node-user <goarch> <syedge tarball>
#       user 模式节点包（不需要 root，设计 0001 第 6 节）：bin/ + sbin/ + modules/ + install-user.sh
#       产物 syedge-node-user-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh pkg <goarch> <syedge tarball>
#       RPM / DEB 软件包（需要 nfpm）：syedge、syedge-agent、syedge-console（控制台须先构建 web/dist）
#
#   scripts/release/package.sh vendor-tools <goos> <goarch>
#       厂商工具（不交付客户）：离线签名工具 syedge-license-signer + 运维手册
#       产物 syedge-vendor-tools-<ver>-<goos>-<goarch>.tar.gz（windows 为 .zip）
#
#   scripts/release/package.sh license-portal <goarch>
#       厂商授权后台（不交付客户，部署在厂商内网）：syedge-license-server（内嵌 Web）+ systemd 单元 + 运维手册
#       只持有签名公钥；产物 syedge-license-portal-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh syedge-upgrade <goarch> <syedge tarball> [standard|gmssl]
#       数据面升级包（规格书 12.6）：sbin/syedge + VERSION，产物 syedge-<ver>-<goarch>.tar.gz（未签名；
#       正式发布前在离线签名机上 syedge-license-signer sign-package 生成 .sig，与包一起导入控制台版本库）
#
#   scripts/release/package.sh lua-upgrade <lua tarball> <包版本> <x86_64|aarch64>
#       为数据面发行版本生成内容相同、版本匹配的 Lua 附加包。
#
#   scripts/release/package.sh agent-upgrade <goarch>
#       agent 自升级包：bin/syedge-agent，产物 syedge-agent-<ver>-<goarch>.tar.gz（未签名）
#
#   scripts/release/package.sh backend <goarch>
#       演示后端（只用于构建演示镜像，不作为发布资产）
#
# 环境变量：VERSION（默认 0.0.0-dev）、COMMIT、BUILD_DATE、OUT（默认 dist/release）、
#           SYEDGE_PREFIX（编译期与默认 system 安装根目录，默认 /opt/<品牌>）、
#           GOTAGS（v* 正式版本为 release：不含演示签名密钥，也不带演示用的 syedge-license-server）
# 正式与演示构建绝不注入 CI 临时授权公钥（license.ciKey）。
# 品牌：读取 BRAND_ENV（默认仓库根目录的 brand.env，设计 0001 第 4 节）。定制品牌时 Go 程序以 -tags brand 注入品牌，
#       暂存目录中的文件名与文本按品牌改写，打包前检查没有残留默认品牌名；默认品牌的产物与以前完全相同。
set -euo pipefail
umask 022
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
VERSION="${VERSION:-0.0.0-dev}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%F)}"
OUT="${OUT:-$ROOT/dist/release}"
mkdir -p "$OUT"; OUT="$(cd "$OUT" && pwd)"
PKG=github.com/xiaoguan521/edgetraffic-os/internal/version
LDFLAGS="-s -w -X $PKG.Version=$VERSION -X $PKG.Commit=$COMMIT -X $PKG.BuildDate=$BUILD_DATE"
GOTAGS="${GOTAGS:-}"
# shellcheck disable=SC1090
source "${BRAND_ENV:-$ROOT/brand.env}"
DEFAULT_BRAND=syedge DEFAULT_PROC=SyEdge DEFAULT_PRODUCT=SyEdge DEFAULT_FULLNAME="神玥企业级动态网关与负载均衡引擎"
B="${BRAND_ID:?brand.env: BRAND_ID required}"; P="${BRAND_PROC:-$B}"; PRODUCT="${BRAND_PRODUCT:-$B}"; FULLNAME="${BRAND_FULLNAME:-$PRODUCT}"
[[ "$B" =~ ^[a-z][a-z0-9]{2,6}$ ]] || { echo "BRAND_ID must match ^[a-z][a-z0-9]{2,6}\$" >&2; exit 2; }
PREFIX="${SYEDGE_PREFIX:-/opt/$B}"
# shellcheck source=../validate-prefix.sh
source "$ROOT/scripts/validate-prefix.sh"
validate_system_prefix "$PREFIX" "/opt/$B" || { echo "SYEDGE_PREFIX must be a safe dedicated absolute directory" >&2; exit 2; }
SLUG="$B"   # 发布包文件名前缀
if [[ "$B" != "$DEFAULT_BRAND" || "$P" != "$DEFAULT_PROC" || "$PRODUCT" != "$DEFAULT_PRODUCT" || "$FULLNAME" != "$DEFAULT_FULLNAME" ]]; then
  BP=github.com/xiaoguan521/edgetraffic-os/internal/brand
  LDFLAGS+=" -X $BP.id=$B -X $BP.proc=$P -X '$BP.product=$PRODUCT' -X '$BP.fullName=$FULLNAME'"
  GOTAGS="${GOTAGS:+$GOTAGS,}brand"
fi
LDFLAGS+=" -X github.com/xiaoguan521/edgetraffic-os/internal/layout.defaultSystemRoot=$PREFIX"
export CGO_ENABLED=0 GOFLAGS=-mod=vendor

gobuild() { # goos goarch outdir pkgs...
  local os=$1 arch=$2 dir=$3; shift 3
  for p in "$@"; do
    local name; name=$(basename "$p"); name=${name//$DEFAULT_BRAND/$B}; [[ $os == windows ]] && name+=.exe
    GOOS=$os GOARCH=$arch go build -trimpath -tags "$GOTAGS" -ldflags "$LDFLAGS" -o "$dir/$name" "$p"
  done
}

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

# rebrand <目录>：把暂存目录中的默认品牌名改为定制品牌（文本内容与文件名）。Go 程序与数据面二进制已按品牌构建，不改动。
rebrand() {
  [[ "$B" == "$DEFAULT_BRAND" ]] && return 0
  local up=${B^^} dup=${DEFAULT_BRAND^^}
  grep -rlIZ -e "$DEFAULT_BRAND" -e "$dup" -e "$DEFAULT_PROC" "$1" | xargs -0 -r sed -i -e "s/$DEFAULT_PROC/$P/g" -e "s/$DEFAULT_BRAND/$B/g" -e "s/$dup/$up/g"
  find "$1" -depth -name "*$DEFAULT_BRAND*" -print0 | while IFS= read -r -d '' p; do
    mv "$p" "$(dirname "$p")/$(basename "$p" | sed "s/$DEFAULT_BRAND/$B/g")"
  done
}

# prefix_template：把构建期默认根目录从模板中的 /opt/syedge 替换为 Action 入参。
# 只处理 shell / systemd / nfpm / 文档模板，不触碰已编译的数据面二进制。
prefix_template() {
  local src=$1 dst=$2
  sed "s|/opt/$DEFAULT_BRAND|$PREFIX|g" "$src" > "$dst"
}

# brand_scan <目录>：定制品牌的产物中不得残留默认品牌名。二进制中只允许 Go 模块路径与协议 / 数据库字段名
# （这些内部标识只在 strings 或崩溃堆栈中可见，见设计 0001 第 4.4 节）。
brand_scan() {
  [[ "$B" == "$DEFAULT_BRAND" ]] && return 0
  local bad
  bad=$(grep -rlIi "$DEFAULT_BRAND" "$1" || true)
  [[ -z "$bad" ]] || { echo "default brand left in packaged text files:" >&2; echo "$bad" >&2; exit 4; }
  bad=$(find "$1" -iname "*$DEFAULT_BRAND*" || true)
  [[ -z "$bad" ]] || { echo "default brand left in file names:" >&2; echo "$bad" >&2; exit 4; }
  while IFS= read -r -d '' f; do
    if strings "$f" | grep -i "$DEFAULT_BRAND" | grep -v 'github.com/xiaoguan521/edgetraffic-os/' \
        | grep -qvi "${DEFAULT_BRAND}_\?version\|${DEFAULT_BRAND}_\?sha256"; then
      echo "default brand left in binary $f" >&2; exit 4
    fi
  done < <(find "$1" -type f -perm -u+x -print0)
  echo "brand scan OK ($B): $1"
}

case "${1:-}" in
  console)
    os=${2:?goos}; arch=${3:?goarch}
    ls web/dist/index.html >/dev/null 2>&1 || { echo "web/dist 未构建：先执行 make web" >&2; exit 2; }
    name="$SLUG-console-$VERSION-$os-$arch"
    d="$WORK/$name"; mkdir -p "$d"
    gobuild "$os" "$arch" "$d" ./cmd/syedge-console
    # 演示脚本在控制台容器里用它签发演示授权；正式版本不带（正式 License 由离线签名机签发）
    [[ "$GOTAGS" == *release* ]] || gobuild "$os" "$arch" "$d" ./cmd/syedge-license-server
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-console.txt "$d/README.txt"
    if [[ $os == windows ]]; then
      (cd "$WORK" && zip -qr "$OUT/$name.zip" "$name")
      echo "$OUT/$name.zip"
    else
      [[ $os == linux ]] && cp deploy/systemd/syedge-console.service "$d/"
      rebrand "$d"; brand_scan "$d"
      tar -C "$WORK" -czf "$OUT/$name.tar.gz" "$name"
      echo "$OUT/$name.tar.gz"
    fi
    ;;
  vendor-tools)
    os=${2:?goos}; arch=${3:?goarch}
    name="$SLUG-vendor-tools-$VERSION-$os-$arch"
    d="$WORK/$name"; mkdir -p "$d"
    gobuild "$os" "$arch" "$d" ./cmd/syedge-license-signer
    cp docs/license-ops.md "$d/license-ops.md"
    cp docs/NOTICE "$d/NOTICE"
    rebrand "$d"; brand_scan "$d"
    if [[ $os == windows ]]; then
      (cd "$WORK" && zip -qr "$OUT/$name.zip" "$name")
      echo "$OUT/$name.zip"
    else
      tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
      echo "$OUT/$name.tar.gz"
    fi
    ;;
  syedge-upgrade)
    arch=${2:?goarch}; syedge_tgz=${3:?syedge tarball}; variant=${4:-standard}
    [[ "$arch" == amd64 || "$arch" == arm64 ]] || { echo "bad architecture $arch" >&2; exit 2; }
    [[ "$variant" == standard || "$variant" == gmssl ]] || { echo "bad variant $variant" >&2; exit 2; }
    x="$WORK/x"; mkdir -p "$x" "$WORK/up/sbin"
    tar -C "$x" -xzf "$syedge_tgz"
    bin=$(find "$x" -type f -path "*/sbin/$B" | head -1)
    [[ -n "$bin" ]] || { echo "no sbin/$B in $syedge_tgz" >&2; exit 2; }
    install -m 0755 "$bin" "$WORK/up/sbin/$B"
    up_ver="${SYEDGE_UPGRADE_VERSION:-$VERSION}"
    [[ "$up_ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || { echo "upgrade version $up_ver is not N.N.N[-N]" >&2; exit 2; }
    echo "$up_ver" > "$WORK/up/VERSION"
    cp docs/NOTICE "$WORK/up/NOTICE"
    rebrand "$WORK/up"; brand_scan "$WORK/up"
    suffix=""; [[ "$variant" == gmssl ]] && suffix=-gmssl
    tar -C "$WORK/up" --owner=0 --group=0 -czf "$OUT/$B-$up_ver$suffix-$arch.tar.gz" "sbin/$B" VERSION NOTICE
    echo "$OUT/$B-$up_ver$suffix-$arch.tar.gz"
    ;;
  lua-upgrade)
    lua_tgz=${2:?lua tarball}; up_ver=${3:?package version}; arch=${4:?architecture}
    [[ "$up_ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || { echo "bad package version $up_ver" >&2; exit 2; }
    [[ "$arch" == x86_64 || "$arch" == aarch64 ]] || { echo "bad architecture $arch" >&2; exit 2; }
    mkdir -p "$WORK/lua"
    tar -C "$WORK/lua" -xzf "$lua_tgz"
    [[ -f "$WORK/lua/addon/VERSION" && -f "$WORK/lua/install.sh" && -f "$WORK/lua/uninstall.sh" ]] || { echo "invalid Lua package" >&2; exit 2; }
    base_ver=$(sed -n 's/.*"syedge":"\([^"]*\)".*/\1/p' "$WORK/lua/addon/VERSION")
    [[ "$base_ver" == "${up_ver%-*}" ]] || { echo "Lua base version $base_ver does not match $up_ver" >&2; exit 2; }
    sed -E "s/\"version\":\"[^\"]+\"/\"version\":\"$up_ver\"/" "$WORK/lua/addon/VERSION" > "$WORK/lua/addon/VERSION.new"
    mv "$WORK/lua/addon/VERSION.new" "$WORK/lua/addon/VERSION"
    if [[ -f "$WORK/lua/addon/NOTICE" ]]; then
      sed "1s/.*/$B-lua $up_ver/" "$WORK/lua/addon/NOTICE" > "$WORK/lua/addon/NOTICE.new"
      mv "$WORK/lua/addon/NOTICE.new" "$WORK/lua/addon/NOTICE"
    fi
    rebrand "$WORK/lua"; brand_scan "$WORK/lua"
    tar -C "$WORK/lua" --owner=0 --group=0 -czf "$OUT/$B-lua-$up_ver-$arch.tar.gz" install.sh uninstall.sh addon
    echo "$OUT/$B-lua-$up_ver-$arch.tar.gz"
    ;;
  agent-upgrade)
    # agent 自升级包：bin/syedge-agent（未签名，离线签名后导入控制台版本库）
    arch=${2:?goarch}
    mkdir -p "$WORK/ag/bin"
    gobuild linux "$arch" "$WORK/ag/bin" ./cmd/syedge-agent
    cp docs/NOTICE "$WORK/ag/NOTICE"
    rebrand "$WORK/ag"; brand_scan "$WORK/ag"
    tar -C "$WORK/ag" --owner=0 --group=0 -czf "$OUT/$B-agent-$VERSION-$arch.tar.gz" "bin/$B-agent" NOTICE
    echo "$OUT/$B-agent-$VERSION-$arch.tar.gz"
    ;;
  license-portal)
    arch=${2:?goarch}
    ls web/portal/dist/index.html >/dev/null 2>&1 || { echo "web/portal/dist 未构建：先执行 make web" >&2; exit 2; }
    name="$SLUG-license-portal-$VERSION-linux-$arch"
    d="$WORK/$name"; mkdir -p "$d"
    gobuild linux "$arch" "$d" ./cmd/syedge-license-server
    cp deploy/systemd/syedge-license-server.service "$d/"
    cp docs/license-ops.md "$d/license-ops.md"
    cp docs/NOTICE "$d/NOTICE"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  node)
    arch=${2:?goarch}; syedge_tgz=${3:?syedge tarball}
    name="$SLUG-node-$VERSION-linux-$arch"
    d="$WORK/$name"; r="$d/rootfs"
    mkdir -p "$r/usr/bin" "$r/usr/sbin" "$r/usr/libexec/syedge" "$r/etc/sudoers.d" "$r/etc/systemd/system" "$r/etc/logrotate.d"
    tar -C "$r" -xzf "$syedge_tgz"
    data_root="$r$PREFIX"
    [[ -x "$data_root/sbin/$B" ]] || { echo "$syedge_tgz 中没有 $PREFIX/sbin/$B" >&2; exit 2; }
    go run -trimpath -tags "$GOTAGS" -ldflags "$LDFLAGS" ./cmd/syedge-agent package-defaults -out "$data_root"   # 初始版本与独立模式示例
    gobuild linux "$arch" "$r/usr/bin" ./cmd/syedge-agent
    gobuild linux "$arch" "$r/usr/libexec/syedge" ./cmd/syedge-wrapper
    gobuild linux "$arch" "$r/usr/sbin" ./cmd/syedge-instance
    cp dataplane/packaging/sudoers/syedge-agent "$r/etc/sudoers.d/syedge-agent"
    for unit in syedge.service syedge@.service syedge-agent.service syedge-agent@.service \
      syedge-firewall.service syedge-firewall-refresh.service syedge-firewall-refresh.path; do
      prefix_template "dataplane/packaging/systemd/$unit" "$r/etc/systemd/system/$unit"
    done
    install -m 0755 dataplane/packaging/firewall/syedge-firewall "$r/usr/sbin/syedge-firewall"
    prefix_template dataplane/packaging/logrotate/syedge "$r/etc/logrotate.d/syedge"
    mkdir -p "$r/etc/tmpfiles.d" && prefix_template dataplane/packaging/tmpfiles/syedge.conf "$r/etc/tmpfiles.d/syedge.conf"
    prefix_template deploy/node/install.sh "$d/install.sh"
    prefix_template deploy/node/uninstall.sh "$d/uninstall.sh"
    cp docs/NOTICE "$d/NOTICE"
    prefix_template scripts/release/README-node.txt "$d/README.txt"
    chmod 0755 "$d/install.sh" "$d/uninstall.sh"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  all)
    # 整合包：节点包 + 国密版二进制 + Lua 附加包，首次安装时由 install.sh --gmssl / --no-lua 选择。
    # 只用于首次安装与手动升级；经控制台升级仍使用厂商签名的单独升级包，整合包不进入验签流程。
    arch=${2:?goarch}; node_tgz=${3:?node tarball}; gm_tgz=${4:?gmssl base tarball}; lua_tgz=${5:?lua addon tarball}
    [[ "$arch" == amd64 || "$arch" == arm64 ]] || { echo "bad architecture $arch" >&2; exit 2; }
    src_name="$SLUG-node-$VERSION-linux-$arch"; name="$SLUG-all-$VERSION-linux-$arch"
    d="$WORK/$name"; x="$WORK/all-src"; g="$WORK/all-gm"; l="$WORK/all-lua"
    mkdir -p "$x" "$g" "$l"
    tar -C "$x" -xzf "$node_tgz"
    [[ -d "$x/$src_name/rootfs" && -f "$x/$src_name/install.sh" ]] || { echo "$node_tgz 不是 $src_name 节点包" >&2; exit 2; }
    mv "$x/$src_name" "$d"
    tar -C "$g" -xzf "$gm_tgz"
    gm_bin=$(find "$g" -type f -path "*/sbin/$B" | head -1)
    [[ -n "$gm_bin" ]] || { echo "$gm_tgz 中没有 sbin/$B" >&2; exit 2; }
    tar -C "$l" -xzf "$lua_tgz"
    [[ -f "$l/addon/VERSION" && -f "$l/install.sh" && -f "$l/uninstall.sh" ]] || { echo "$lua_tgz 不是 Lua 附加包" >&2; exit 2; }
    mkdir -p "$d/extras/gmssl" "$d/extras/lua"
    install -m 0755 "$gm_bin" "$d/extras/gmssl/$B"
    cp -r "$l/install.sh" "$l/uninstall.sh" "$l/addon" "$d/extras/lua/"
    prefix_template scripts/release/README-all.txt "$d/README.txt"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  node-user)
    arch=${2:?goarch}; syedge_tgz=${3:?syedge tarball}
    name="$SLUG-node-user-$VERSION-linux-$arch"
    d="$WORK/$name"; x="$WORK/node-user-src"
    mkdir -p "$d/bin" "$d/sbin" "$d/modules" "$x"
    tar -C "$x" -xzf "$syedge_tgz"
    data_root="$x$PREFIX"
    [[ -x "$data_root/sbin/$B" ]] || { echo "$syedge_tgz 中没有 $PREFIX/sbin/$B" >&2; exit 2; }
    cp -p "$data_root/sbin/$B" "$d/sbin/$B"
    if compgen -G "$data_root/modules/*" >/dev/null; then cp -p "$data_root/modules/"* "$d/modules/"; fi
    gobuild linux "$arch" "$d/bin" ./cmd/syedge-agent ./cmd/syedge-wrapper ./cmd/syedge-instance
    cp deploy/node/install-user.sh "$d/install-user.sh"
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-node-user.txt "$d/README.txt"
    chmod 0755 "$d/install-user.sh"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  pkg)
    arch=${2:?goarch}; syedge_tgz=${3:?syedge tarball}
    command -v nfpm >/dev/null || { echo "需要 nfpm：go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0" >&2; exit 2; }
    ls web/dist/index.html >/dev/null 2>&1 || { echo "web/dist 未构建：先执行 make web" >&2; exit 2; }
    st="$WORK/stage"
    mkdir -p "$st/syedge" "$st/agent" "$st/console" "$st/scripts"
    tar -C "$st/syedge" -xzf "$syedge_tgz"
    data_root="$st/syedge$PREFIX"
    [[ -x "$data_root/sbin/$B" ]] || { echo "$syedge_tgz 中没有 $PREFIX/sbin/$B" >&2; exit 2; }
    gobuild linux "$arch" "$st/syedge" ./cmd/syedge-instance
    go run -trimpath -tags "$GOTAGS" -ldflags "$LDFLAGS" ./cmd/syedge-agent package-defaults -out "$data_root"   # 数据面软件包可独立使用
    prefix_template dataplane/packaging/systemd/syedge.service "$st/syedge/syedge.service"
    prefix_template dataplane/packaging/systemd/syedge@.service "$st/syedge/syedge@.service"
    prefix_template dataplane/packaging/logrotate/syedge "$st/syedge/logrotate"
    prefix_template dataplane/packaging/tmpfiles/syedge.conf "$st/syedge/tmpfiles.conf"
    gobuild linux "$arch" "$st/agent" ./cmd/syedge-agent ./cmd/syedge-wrapper ./cmd/syedge-instance
    cp dataplane/packaging/sudoers/syedge-agent "$st/agent/sudoers"
    prefix_template dataplane/packaging/systemd/syedge-agent.service "$st/agent/syedge-agent.service"
    prefix_template dataplane/packaging/systemd/syedge-agent@.service "$st/agent/syedge-agent@.service"
    prefix_template dataplane/packaging/systemd/syedge-firewall.service "$st/agent/syedge-firewall.service"
    prefix_template dataplane/packaging/systemd/syedge-firewall-refresh.service "$st/agent/syedge-firewall-refresh.service"
    prefix_template dataplane/packaging/systemd/syedge-firewall-refresh.path "$st/agent/syedge-firewall-refresh.path"
    install -m 0755 dataplane/packaging/firewall/syedge-firewall "$st/agent/syedge-firewall"
    gobuild linux "$arch" "$st/console" ./cmd/syedge-console
    cp deploy/systemd/syedge-console.service deploy/nfpm/console.env docs/NOTICE "$st/console/"
    # 每个脚本 = shebang + 共用函数 + 本体（软件包脚本不能引用包外文件）
    for f in deploy/nfpm/scripts/*.sh; do
      [[ $(basename "$f") == common.sh ]] && continue
      { echo '#!/bin/sh'; cat deploy/nfpm/scripts/common.sh; cat "$f"; } | \
        sed "s|/opt/$DEFAULT_BRAND|$PREFIX|g" > "$st/scripts/$(basename "$f")"
      chmod 0755 "$st/scripts/$(basename "$f")"
    done
    # 先把 ${VERSION} / ${ARCH} / ${STAGE} 代入配置，不依赖 nfpm 对各字段的环境变量展开
    for p in syedge syedge-agent syedge-console; do
      tmp="$WORK/$p.yaml.prefix"
      prefix_template "deploy/nfpm/$p.yaml" "$tmp"
      sed -e "s|\${VERSION}|$VERSION|g" -e "s|\${ARCH}|$arch|g" -e "s|\${STAGE}|$st|g" "$tmp" > "$st/$p.yaml"
    done
    rebrand "$st"; brand_scan "$st"
    for fmt in deb rpm; do
      for p in "$B" "$B-agent" "$B-console"; do
        if ! log=$(nfpm package -f "$st/$p.yaml" -p "$fmt" -t "$OUT/" 2>&1); then
          echo "nfpm $p $fmt failed:" >&2; echo "$log" >&2
          [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::error title=nfpm $p $fmt::$(echo "$log" | tail -5 | tr '\n' ' ')"
          exit 1
        fi
      done
    done
    ls "$OUT"/*.deb "$OUT"/*.rpm
    ;;
  backend)
    arch=${2:?goarch}
    mkdir -p "$OUT/backend-linux-$arch"
    GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$OUT/backend-linux-$arch/demo-backend" ./demo/backend
    echo "$OUT/backend-linux-$arch/demo-backend"
    ;;
  *)
    sed -n '2,23p' "$0"; exit 2 ;;
esac
