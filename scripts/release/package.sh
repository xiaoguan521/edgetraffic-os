#!/usr/bin/env bash
# 发布打包：交叉编译 Go 程序并生成可直接下载使用的发布包。
#
#   scripts/release/package.sh console <goos> <goarch>
#       控制台包：syapp-console（前端已嵌入，须先构建 web/dist）；开发构建另带演示用的 syapp-license-server
#       产物 edgetraffic-os-console-<ver>-<goos>-<goarch>.tar.gz（windows 为 .zip）
#
#   scripts/release/package.sh node <goarch> <syapp tarball>
#       节点包（仅 Linux）：syapp 数据面 + syapp-agent + syapp-wrapper + systemd 单元 + install.sh
#       产物 edgetraffic-os-node-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh node-user <goarch> <syapp tarball>
#       user 模式节点包（不需要 root，设计 0001 第 6 节）：bin/ + sbin/ + modules/ + install-user.sh
#       产物 edgetraffic-os-node-user-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh pkg <goarch> <syapp tarball>
#       RPM / DEB 软件包（需要 nfpm）：syapp、syapp-agent、syapp-console（控制台须先构建 web/dist）
#
#   scripts/release/package.sh vendor-tools <goos> <goarch>
#       厂商工具（不交付客户）：离线签名工具 syapp-license-signer + 运维手册
#       产物 edgetraffic-os-vendor-tools-<ver>-<goos>-<goarch>.tar.gz（windows 为 .zip）
#
#   scripts/release/package.sh license-portal <goarch>
#       厂商授权后台（不交付客户，部署在厂商内网）：syapp-license-server（内嵌 Web）+ systemd 单元 + 运维手册
#       只持有签名公钥；产物 edgetraffic-os-license-portal-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh syapp-upgrade <goarch> <syapp tarball>
#       数据面升级包（规格书 12.6）：sbin/syapp + VERSION，产物 syapp-<ver>-<goarch>.tar.gz（未签名；
#       正式发布前在离线签名机上 syapp-license-signer sign-package 生成 .sig，与包一起导入控制台版本库）
#
#   scripts/release/package.sh agent-upgrade <goarch>
#       agent 自升级包：bin/syapp-agent，产物 syapp-agent-<ver>-<goarch>.tar.gz（未签名）
#
#   scripts/release/package.sh backend <goarch>
#       演示后端（只用于构建演示镜像，不作为发布资产）
#
# 环境变量：VERSION（默认 0.0.0-dev）、COMMIT、BUILD_DATE、OUT（默认 dist/release）、
#           GOTAGS（v* 正式版本为 release：不含演示签名密钥，也不带演示用的 syapp-license-server）
# 正式与演示构建绝不注入 CI 临时授权公钥（license.ciKey）。
# 品牌：读取 BRAND_ENV（默认仓库根目录的 brand.env，设计 0001 第 4 节）。定制品牌时 Go 程序以 -tags brand 注入品牌，
#       暂存目录中的文件名与文本按品牌改写，打包前检查没有残留默认品牌名；默认品牌的产物与以前完全相同。
set -euo pipefail
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
DEFAULT_BRAND=syapp
B="${BRAND_ID:?brand.env: BRAND_ID required}"; P="${BRAND_PROC:-$B}"; PRODUCT="${BRAND_PRODUCT:-EdgeTraffic OS}"
[[ "$B" =~ ^[a-z][a-z0-9]{2,6}$ ]] || { echo "BRAND_ID must match ^[a-z][a-z0-9]{2,6}\$" >&2; exit 2; }
SLUG=edgetraffic-os   # 发布包文件名前缀
if [[ "$B" != "$DEFAULT_BRAND" || "$P" != "$DEFAULT_BRAND" || "$PRODUCT" != "EdgeTraffic OS" ]]; then
  BP=github.com/xiaoguan521/edgetraffic-os/internal/brand
  LDFLAGS+=" -X $BP.id=$B -X $BP.proc=$P -X '$BP.product=$PRODUCT'"
  GOTAGS="${GOTAGS:+$GOTAGS,}brand"
fi
[[ "$B" != "$DEFAULT_BRAND" ]] && SLUG="$B"
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
  grep -rlIZ -e "$DEFAULT_BRAND" -e "$dup" "$1" | xargs -0 -r sed -i -e "s/$DEFAULT_BRAND/$B/g" -e "s/$dup/$up/g"
  find "$1" -depth -name "*$DEFAULT_BRAND*" -print0 | while IFS= read -r -d '' p; do
    mv "$p" "$(dirname "$p")/$(basename "$p" | sed "s/$DEFAULT_BRAND/$B/g")"
  done
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
    gobuild "$os" "$arch" "$d" ./cmd/syapp-console
    # 演示脚本在控制台容器里用它签发演示授权；正式版本不带（正式 License 由离线签名机签发）
    [[ "$GOTAGS" == *release* ]] || gobuild "$os" "$arch" "$d" ./cmd/syapp-license-server
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-console.txt "$d/README.txt"
    if [[ $os == windows ]]; then
      (cd "$WORK" && zip -qr "$OUT/$name.zip" "$name")
      echo "$OUT/$name.zip"
    else
      [[ $os == linux ]] && cp deploy/systemd/syapp-console.service "$d/"
      rebrand "$d"; brand_scan "$d"
      tar -C "$WORK" -czf "$OUT/$name.tar.gz" "$name"
      echo "$OUT/$name.tar.gz"
    fi
    ;;
  vendor-tools)
    os=${2:?goos}; arch=${3:?goarch}
    name="$SLUG-vendor-tools-$VERSION-$os-$arch"
    d="$WORK/$name"; mkdir -p "$d"
    gobuild "$os" "$arch" "$d" ./cmd/syapp-license-signer
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
  syapp-upgrade)
    arch=${2:?goarch}; syapp_tgz=${3:?syapp tarball}
    x="$WORK/x"; mkdir -p "$x" "$WORK/up/sbin"
    tar -C "$x" -xzf "$syapp_tgz"
    bin=$(find "$x" -type f -path "*/sbin/$B" | head -1)
    [[ -n "$bin" ]] || { echo "no sbin/$B in $syapp_tgz" >&2; exit 2; }
    install -m 0755 "$bin" "$WORK/up/sbin/$B"
    up_ver="${SYAPP_UPGRADE_VERSION:-$VERSION}"
    [[ "$up_ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+)?$ ]] || { echo "upgrade version $up_ver is not N.N.N[-N]" >&2; exit 2; }
    echo "$up_ver" > "$WORK/up/VERSION"
    cp docs/NOTICE "$WORK/up/NOTICE"
    rebrand "$WORK/up"; brand_scan "$WORK/up"
    tar -C "$WORK/up" --owner=0 --group=0 -czf "$OUT/$B-$up_ver-$arch.tar.gz" "sbin/$B" VERSION NOTICE
    echo "$OUT/$B-$up_ver-$arch.tar.gz"
    ;;
  agent-upgrade)
    # agent 自升级包：bin/syapp-agent（未签名，离线签名后导入控制台版本库）
    arch=${2:?goarch}
    mkdir -p "$WORK/ag/bin"
    gobuild linux "$arch" "$WORK/ag/bin" ./cmd/syapp-agent
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
    gobuild linux "$arch" "$d" ./cmd/syapp-license-server
    cp deploy/systemd/syapp-license-server.service "$d/"
    cp docs/license-ops.md "$d/license-ops.md"
    cp docs/NOTICE "$d/NOTICE"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  node)
    arch=${2:?goarch}; syapp_tgz=${3:?syapp tarball}
    name="$SLUG-node-$VERSION-linux-$arch"
    d="$WORK/$name"; r="$d/rootfs"
    mkdir -p "$r/usr/bin" "$r/usr/sbin" "$r/usr/libexec/syapp" "$r/etc/sudoers.d" "$r/etc/systemd/system" "$r/etc/logrotate.d"
    tar -C "$r" -xzf "$syapp_tgz"
    [[ -x "$r/opt/$B/sbin/$B" ]] || { echo "$syapp_tgz 中没有 opt/$B/sbin/$B" >&2; exit 2; }
    go run -trimpath -tags "$GOTAGS" -ldflags "$LDFLAGS" ./cmd/syapp-agent package-defaults -out "$r/opt/$B"   # 初始版本与独立模式示例
    gobuild linux "$arch" "$r/usr/bin" ./cmd/syapp-agent
    gobuild linux "$arch" "$r/usr/libexec/syapp" ./cmd/syapp-wrapper
    gobuild linux "$arch" "$r/usr/sbin" ./cmd/syapp-instance
    cp dataplane/packaging/sudoers/syapp-agent "$r/etc/sudoers.d/syapp-agent"
    cp dataplane/packaging/systemd/syapp.service dataplane/packaging/systemd/syapp@.service \
       dataplane/packaging/systemd/syapp-agent.service dataplane/packaging/systemd/syapp-agent@.service \
       dataplane/packaging/systemd/syapp-firewall.service "$r/etc/systemd/system/"
    install -m 0755 dataplane/packaging/firewall/syapp-firewall "$r/usr/sbin/syapp-firewall"
    cp dataplane/packaging/logrotate/syapp "$r/etc/logrotate.d/syapp"
    mkdir -p "$r/etc/tmpfiles.d" && cp dataplane/packaging/tmpfiles/syapp.conf "$r/etc/tmpfiles.d/syapp.conf"
    cp deploy/node/install.sh "$d/install.sh"
    cp deploy/node/uninstall.sh "$d/uninstall.sh"
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-node.txt "$d/README.txt"
    chmod 0755 "$d/install.sh" "$d/uninstall.sh"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  node-user)
    arch=${2:?goarch}; syapp_tgz=${3:?syapp tarball}
    name="$SLUG-node-user-$VERSION-linux-$arch"
    d="$WORK/$name"; x="$WORK/node-user-src"
    mkdir -p "$d/bin" "$d/sbin" "$d/modules" "$x"
    tar -C "$x" -xzf "$syapp_tgz"
    [[ -x "$x/opt/$B/sbin/$B" ]] || { echo "$syapp_tgz 中没有 opt/$B/sbin/$B" >&2; exit 2; }
    cp -p "$x/opt/$B/sbin/$B" "$d/sbin/$B"
    if compgen -G "$x/opt/$B/modules/*" >/dev/null; then cp -p "$x/opt/$B/modules/"* "$d/modules/"; fi
    gobuild linux "$arch" "$d/bin" ./cmd/syapp-agent ./cmd/syapp-wrapper ./cmd/syapp-instance
    cp deploy/node/install-user.sh "$d/install-user.sh"
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-node-user.txt "$d/README.txt"
    chmod 0755 "$d/install-user.sh"
    rebrand "$d"; brand_scan "$d"
    tar -C "$WORK" --owner=0 --group=0 -czf "$OUT/$name.tar.gz" "$name"
    echo "$OUT/$name.tar.gz"
    ;;
  pkg)
    arch=${2:?goarch}; syapp_tgz=${3:?syapp tarball}
    command -v nfpm >/dev/null || { echo "需要 nfpm：go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0" >&2; exit 2; }
    ls web/dist/index.html >/dev/null 2>&1 || { echo "web/dist 未构建：先执行 make web" >&2; exit 2; }
    st="$WORK/stage"
    mkdir -p "$st/syapp" "$st/agent" "$st/console" "$st/scripts"
    tar -C "$st/syapp" -xzf "$syapp_tgz"
    [[ -x "$st/syapp/opt/$B/sbin/$B" ]] || { echo "$syapp_tgz 中没有 opt/$B/sbin/$B" >&2; exit 2; }
    go run -trimpath -tags "$GOTAGS" -ldflags "$LDFLAGS" ./cmd/syapp-agent package-defaults -out "$st/syapp/opt/$B"   # 数据面软件包可独立使用
    cp dataplane/packaging/systemd/syapp.service "$st/syapp/syapp.service"
    cp dataplane/packaging/systemd/syapp@.service "$st/syapp/syapp@.service"
    cp dataplane/packaging/logrotate/syapp "$st/syapp/logrotate"
    cp dataplane/packaging/tmpfiles/syapp.conf "$st/syapp/tmpfiles.conf"
    gobuild linux "$arch" "$st/agent" ./cmd/syapp-agent ./cmd/syapp-wrapper ./cmd/syapp-instance
    cp dataplane/packaging/sudoers/syapp-agent "$st/agent/sudoers"
    cp dataplane/packaging/systemd/syapp-agent.service "$st/agent/syapp-agent.service"
    cp dataplane/packaging/systemd/syapp-agent@.service "$st/agent/syapp-agent@.service"
    cp dataplane/packaging/systemd/syapp-firewall.service "$st/agent/syapp-firewall.service"
    install -m 0755 dataplane/packaging/firewall/syapp-firewall "$st/agent/syapp-firewall"
    gobuild linux "$arch" "$st/console" ./cmd/syapp-console
    cp deploy/systemd/syapp-console.service deploy/nfpm/console.env docs/NOTICE "$st/console/"
    # 每个脚本 = shebang + 共用函数 + 本体（软件包脚本不能引用包外文件）
    for f in deploy/nfpm/scripts/*.sh; do
      [[ $(basename "$f") == common.sh ]] && continue
      { echo '#!/bin/sh'; cat deploy/nfpm/scripts/common.sh; cat "$f"; } > "$st/scripts/$(basename "$f")"
      chmod 0755 "$st/scripts/$(basename "$f")"
    done
    # 先把 ${VERSION} / ${ARCH} / ${STAGE} 代入配置，不依赖 nfpm 对各字段的环境变量展开
    for p in syapp syapp-agent syapp-console; do
      sed -e "s|\${VERSION}|$VERSION|g" -e "s|\${ARCH}|$arch|g" -e "s|\${STAGE}|$st|g" "deploy/nfpm/$p.yaml" > "$st/$p.yaml"
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
