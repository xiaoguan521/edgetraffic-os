#!/usr/bin/env bash
# 发布打包：交叉编译 Go 程序并生成可直接下载使用的发布包。
#
#   scripts/release/package.sh console <goos> <goarch>
#       控制台包：syapp-console + syapp-license-server（前端已嵌入，须先构建 web/dist）
#       产物 edgetraffic-os-console-<ver>-<goos>-<goarch>.tar.gz（windows 为 .zip）
#
#   scripts/release/package.sh node <goarch> <syapp tarball>
#       节点包（仅 Linux）：syapp 数据面 + syapp-agent + syapp-wrapper + systemd 单元 + install.sh
#       产物 edgetraffic-os-node-<ver>-linux-<goarch>.tar.gz
#
#   scripts/release/package.sh pkg <goarch> <syapp tarball>
#       RPM / DEB 软件包（需要 nfpm）：syapp、syapp-agent、syapp-console（控制台须先构建 web/dist）
#
#   scripts/release/package.sh backend <goarch>
#       演示后端（只用于构建演示镜像，不作为发布资产）
#
# 环境变量：VERSION（默认 0.0.0-dev）、COMMIT、BUILD_DATE、OUT（默认 dist/release）
# 正式与演示构建绝不注入 CI 临时授权公钥（license.ciKey）。
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
export CGO_ENABLED=0 GOFLAGS=-mod=vendor

gobuild() { # goos goarch outdir pkgs...
  local os=$1 arch=$2 dir=$3; shift 3
  for p in "$@"; do
    local name; name=$(basename "$p"); [[ $os == windows ]] && name+=.exe
    GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$dir/$name" "$p"
  done
}

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

case "${1:-}" in
  console)
    os=${2:?goos}; arch=${3:?goarch}
    ls web/dist/index.html >/dev/null 2>&1 || { echo "web/dist 未构建：先执行 make web" >&2; exit 2; }
    name="edgetraffic-os-console-$VERSION-$os-$arch"
    d="$WORK/$name"; mkdir -p "$d"
    gobuild "$os" "$arch" "$d" ./cmd/syapp-console ./cmd/syapp-license-server
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-console.txt "$d/README.txt"
    if [[ $os == windows ]]; then
      (cd "$WORK" && zip -qr "$OUT/$name.zip" "$name")
      echo "$OUT/$name.zip"
    else
      [[ $os == linux ]] && cp deploy/systemd/syapp-console.service "$d/"
      tar -C "$WORK" -czf "$OUT/$name.tar.gz" "$name"
      echo "$OUT/$name.tar.gz"
    fi
    ;;
  node)
    arch=${2:?goarch}; syapp_tgz=${3:?syapp tarball}
    name="edgetraffic-os-node-$VERSION-linux-$arch"
    d="$WORK/$name"; r="$d/rootfs"
    mkdir -p "$r/usr/bin" "$r/usr/sbin" "$r/usr/libexec/syapp" "$r/etc/sudoers.d" "$r/etc/systemd/system" "$r/etc/logrotate.d"
    tar -C "$r" -xzf "$syapp_tgz"
    [[ -x "$r/opt/syapp/sbin/syapp" ]] || { echo "$syapp_tgz 中没有 opt/syapp/sbin/syapp" >&2; exit 2; }
    gobuild linux "$arch" "$r/usr/bin" ./cmd/syapp-agent
    gobuild linux "$arch" "$r/usr/libexec/syapp" ./cmd/syapp-wrapper
    cp dataplane/packaging/sudoers/syapp-agent "$r/etc/sudoers.d/syapp-agent"
    cp dataplane/packaging/systemd/syapp.service dataplane/packaging/systemd/syapp-agent.service \
       dataplane/packaging/systemd/syapp-firewall.service "$r/etc/systemd/system/"
    install -m 0755 dataplane/packaging/firewall/syapp-firewall "$r/usr/sbin/syapp-firewall"
    cp dataplane/packaging/logrotate/syapp "$r/etc/logrotate.d/syapp"
    cp deploy/node/install.sh "$d/install.sh"
    cp docs/NOTICE "$d/NOTICE"
    cp scripts/release/README-node.txt "$d/README.txt"
    chmod 0755 "$d/install.sh"
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
    cp dataplane/packaging/systemd/syapp.service "$st/syapp/syapp.service"
    cp dataplane/packaging/logrotate/syapp "$st/syapp/logrotate"
    gobuild linux "$arch" "$st/agent" ./cmd/syapp-agent ./cmd/syapp-wrapper
    cp dataplane/packaging/sudoers/syapp-agent "$st/agent/sudoers"
    cp dataplane/packaging/systemd/syapp-agent.service "$st/agent/syapp-agent.service"
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
    for fmt in deb rpm; do
      for p in syapp syapp-agent syapp-console; do
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
    sed -n '2,19p' "$0"; exit 2 ;;
esac
