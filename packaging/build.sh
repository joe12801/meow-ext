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

# 必须用官方 fnpack 打包，不要手写 tar。
# fnpack 是飞牛官方的打包/校验工具，应用中心用它同一套规则验包：
# manifest 的 checksum、包内条目名、wizard/*.json 的 schema 全由它把关。
# 手写 tar 能凑出结构，但 wizard/install 里一个非法字段（例如
# "type": "input"，合法值是 "text"）会让应用中心在解包阶段直接拒绝，
# 报的却是「应用包不符合应用/系统要求」，完全看不出是哪个文件的问题；
# 而 fnpack 会在打包期就精确报出 "File \"install\" is not valid ..."。
ensure_fnpack() {
    if command -v fnpack >/dev/null 2>&1; then
        command -v fnpack
        return
    fi
    local cache="${ROOT}/.tools" ver="${FNPACK_VERSION:-1.2.4}" arch bin
    case "$(uname -m)" in
        x86_64|amd64)  arch=amd64 ;;
        aarch64|arm64) arch=arm ;;
        *) die "不支持的构建机架构：$(uname -m)" ;;
    esac
    mkdir -p "${cache}"
    bin="${cache}/fnpack"
    if [ ! -x "${bin}" ]; then
        printf '\033[1;34m[build]\033[0m 本机没有 fnpack，下载 %s（linux-%s）\n' "${ver}" "${arch}" >&2
        curl -fsSL -o "${bin}" \
            "https://static2.fnnas.com/fnpack/fnpack-${ver}-linux-${arch}" \
            || die "下载 fnpack 失败，请手动安装 fnpack 后重试"
        chmod +x "${bin}"
    fi
    echo "${bin}"
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

    # ---- 2. app.tgz 交给 fnpack 生成 ----
    # app/ 保持目录形态（bin、ui、systemd），fnpack 会自动打成包内顶层
    # 唯一的 app.tgz，并按它的 md5 写好 manifest 的 checksum。

    # ---- 3. manifest（checksum 由 fnpack 打包时写入，这里不要自己算）----
    sed -e "s/@VERSION@/${VERSION}/g" \
        -e "s/@PLATFORM@/$(platform_of "${arch}")/g" \
        packaging/fpk/manifest.in > "${stage}/manifest"
    cp packaging/fpk/ICON.PNG packaging/fpk/ICON_256.PNG "${stage}/"
    cp -a packaging/fpk/config/. "${stage}/config/"
    cp -a packaging/fpk/cmd/. "${stage}/cmd/"
    cp -a packaging/fpk/wizard/. "${stage}/wizard/"
    # 生命周期脚本必须可执行，否则应用中心会静默跳过它们。
    chmod 0755 "${stage}/cmd/"* 2>/dev/null || true

    # ---- 4. 打包前自检：这几项缺任何一项都会被应用中心拒装 ----
    for _need in manifest ICON.PNG ICON_256.PNG cmd/main config/privilege app/bin/meow-ext; do
        [ -e "${stage}/${_need}" ] || die "FPK 结构不完整，缺少 ${_need}"
    done

    # ---- 5. 用官方 fnpack 打包 ----
    # 所有格式细节（app.tgz、checksum、条目名、wizard schema）都由它保证，
    # 手写 tar 做不到同等校验收敛。
    pkg="${OUT}/meow-ext-${VERSION}-${arch}.fpk"
    rm -f "${pkg}"
    _work="$(mktemp -d)"
    if ! ( cd "${_work}" && "$(ensure_fnpack)" build -d "${stage}" ); then
        rm -rf "${_work}"
        die "fnpack 打包失败（真实原因见上一行提示）"
    fi
    _built="$(find "${_work}" -maxdepth 1 -name '*.fpk' -print -quit)"
    if [ -z "${_built}" ]; then
        rm -rf "${_work}"
        die "fnpack 没有产出 .fpk"
    fi
    mv "${_built}" "${pkg}"
    rm -rf "${_work}" "${stage}"

    # ---- 6. 产物自检：顶层条目不带 ./ 前缀、不重复、app.tgz 在顶层 ----
    # 注意这里刻意不用 `tar tzf | grep -q`：grep -q 命中即退出会让 tar 收到
    # SIGPIPE，而 pipefail 会把它当成失败，反而误报。
    _list="$(tar tzf "${pkg}" 2>/dev/null || true)"
    case $'\n'"${_list}"$'\n' in
        *$'\n\./'*) die "FPK 条目带了 ./ 前缀，应用中心会拒装" ;;
    esac
    case $'\n'"${_list}"$'\n' in
        *$'\n'app.tgz$'\n'*) ;;
        *) die "FPK 顶层缺少 app.tgz" ;;
    esac
    _dup=$(printf '%s\n' "${_list}" | sort | uniq -d)
    [ -z "${_dup}" ] || die "FPK 里有重复条目：${_dup}"
    log "产出 $(du -h "${pkg}" | cut -f1) ${pkg}"
done

log "全部完成，产物在 ${OUT}"
ls -lh "${OUT}"/*.fpk 2>/dev/null || true
