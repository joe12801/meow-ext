#!/bin/bash
# 打包脚本：交叉编译单二进制 → 组装 FPK。
#
# 优先用本机 go；没有就用 docker 里的 golang 镜像（构建机常见情况）。
# 产物：
#   dist/meow-ext-<版本>-arm64.fpk
#   dist/meow-ext-<版本>-amd64.fpk
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

VERSION="${VERSION:-$(cat VERSION 2>/dev/null || echo 0.1.0)}"
OUT="${OUT:-${ROOT}/dist}"
PLATFORMS="${PLATFORMS:-arm64 amd64}"
GO_IMAGE="${GO_IMAGE:-golang:1.25-alpine}"

# 关闭 CGO 是刻意的：静态二进制在飞牛的任意 glibc 版本上都能跑，
# 也不会因为 NAS 上缺某个 .so 而在启动时炸掉。
LDFLAGS="-s -w -X main.Version=${VERSION}"

log() { printf '\033[1;34m[build]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[build]\033[0m %s\n' "$*" >&2; exit 1; }

have_go() { command -v go >/dev/null 2>&1; }
have_docker() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

build_binary() {
    local goarch="$1" out="$2"
    if have_go; then
        CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" \
            go build -trimpath -buildvcs=false -ldflags "${LDFLAGS}" -o "${out}" .
    elif have_docker; then
        # 复用命名卷当模块缓存，避免每次打包都重编标准库。
        docker run --rm \
            -v "${ROOT}":/src -w /src \
            -v meow-go-cache:/go \
            -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="${goarch}" \
            "${GO_IMAGE}" \
            go build -trimpath -buildvcs=false -ldflags "${LDFLAGS}" -o "/src/dist/bin/meow-ext-${goarch}" .
    else
        die "既没有 go 也没有可用的 docker，无法编译"
    fi
}

platform_of() {
    case "$1" in
        arm64|aarch64) echo "arm" ;;
        amd64|x86_64)  echo "x86" ;;
        *) die "不支持的架构：$1" ;;
    esac
}

mkdir -p "${OUT}/bin"
rm -rf "${OUT:?}/stage-"*

for arch in ${PLATFORMS}; do
    log "编译 ${arch}（版本 ${VERSION}）"
    bin="${OUT}/bin/meow-ext-${arch}"
    build_binary "${arch}" "${bin}"
    [ -s "${bin}" ] || die "编译产物为空：${bin}"
    log "$(du -h "${bin}" | cut -f1) ${bin}"

    stage="${OUT}/stage-${arch}"
    mkdir -p "${stage}/config" "${stage}/cmd" "${stage}/wizard" "${stage}/app"

    # ---- 1. 组装应用主体（将来打成 app.tgz，解压到 TRIM_APPDEST）----
    # 布局：app/bin/meow-ext、app/ui/、app/systemd/
    # 解压后即 target/bin/meow-ext，与 manifest 的 desktop_uidir="ui" 对应。
    mkdir -p "${stage}/app/ui/images" "${stage}/app/bin" "${stage}/app/systemd"
    cp -a packaging/fpk/app/ui/. "${stage}/app/ui/"
    cp "${bin}" "${stage}/app/bin/meow-ext"
    chmod 0755 "${stage}/app/bin/meow-ext"
    cp packaging/systemd/meow-ext.service.in "${stage}/app/systemd/"

    # 桌面图标两档尺寸由飞牛桌面按需取用。
    if [ -f packaging/fpk/icons/icon_64.png ]; then
        cp packaging/fpk/icons/icon_64.png "${stage}/app/ui/images/icon_64.png"
        cp packaging/fpk/icons/icon_256.png "${stage}/app/ui/images/icon_256.png"
    else
        log "警告：缺少 app/ui/images 图标，桌面图标会显示为空白"
    fi

    # ---- 2. 打成 app.tgz ----
    # 这是飞牛 FPK 的硬性约定：应用主体必须是包内顶层的一个 app.tgz，
    # 缺了它应用中心会直接报 ErrCodePackageException（"应用包不符合系统要求"）。
    # 用 -C app . 让归档内顶层就是 bin/ ui/ systemd/，解压后直接落在 target/ 下。
    tar czf "${stage}/app.tgz" -C "${stage}/app" .
    rm -rf "${stage}/app"

    # ---- 3. manifest（checksum 必须是 app.tgz 的 md5，构建期算完注入）----
    # 校验和是安装前的第一道关：对不上就说明包坏了或被人改过，应用中心会拒装。
    checksum="$(md5sum "${stage}/app.tgz" | cut -d' ' -f1)"
    sed -e "s/@VERSION@/${VERSION}/g" \
        -e "s/@PLATFORM@/$(platform_of "${arch}")/g" \
        -e "s/@CHECKSUM@/${checksum}/g" \
        packaging/fpk/manifest.in > "${stage}/manifest"
    cp packaging/fpk/ICON.PNG packaging/fpk/ICON_256.PNG "${stage}/"
    cp -a packaging/fpk/config/. "${stage}/config/"
    cp -a packaging/fpk/cmd/. "${stage}/cmd/"
    cp -a packaging/fpk/wizard/. "${stage}/wizard/"
    # 生命周期脚本必须可执行，否则应用中心会静默跳过它们。
    chmod 0755 "${stage}/cmd/"* 2>/dev/null || true

    # ---- 4. 打包前自检：这几项缺任何一项都会被应用中心拒装 ----
    for _need in manifest app.tgz ICON.PNG ICON_256.PNG cmd/main config/privilege; do
        [ -e "${stage}/${_need}" ] || die "FPK 结构不完整，缺少 ${_need}"
    done
    _sz=$(wc -c < "${stage}/app.tgz")
    # app.tgz 过小说明打包中断（真实应用至少几百 KB）。
    [ "${_sz}" -gt 10240 ] || die "app.tgz 只有 ${_sz} 字节，明显不完整"

    pkg="${OUT}/meow-ext-${VERSION}-${arch}.fpk"
    rm -f "${pkg}"
    # FPK 就是 tar.gz，只是扩展名不同；用 -9 压，包里九成是二进制，压得动。
    tar czf "${pkg}" -C "${stage}" .
    log "产出 $(du -h "${pkg}" | cut -f1) ${pkg}"
    rm -rf "${stage}"
done

log "全部完成，产物在 ${OUT}"
ls -lh "${OUT}"/*.fpk 2>/dev/null || true
