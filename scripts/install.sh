#!/bin/bash
# 命令行安装（不经过应用中心）。适合排障、自定义路径、或想把服务装在非默认位置。
#
#   sudo ./scripts/install.sh --meow-url http://192.168.2.220:2233
#   sudo ./scripts/install.sh --uninstall
set -euo pipefail

PREFIX="${PREFIX:-/opt/meow-ext}"
BIN_DIR="${PREFIX}/bin"
CONFIG_DIR="${CONFIG_DIR:-/etc/meow-ext}"
CONFIG_FILE="${CONFIG_DIR}/config.json"
DATA_DIR="${DATA_DIR:-/var/lib/meow-ext}"
STATE_DIR="/run/meow-ext"
UNIT="/etc/systemd/system/meow-ext.service"
TARGET_SOCKET="/var/run/trim_music.socket"
MEOW_URL="http://127.0.0.1:2233"
UNINSTALL=0

log() { printf '\033[1;34m[meow-ext]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[meow-ext]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'
    cat <<'EOF'
选项：
  --meow-url <url>    MeowMusicServer 地址（默认 http://127.0.0.1:2233）
  --prefix <dir>      安装前缀（默认 /opt/meow-ext）
  --uninstall         卸载：还原官方入口并移除服务
  -h, --help          显示本帮助
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --meow-url) MEOW_URL="${2:-}"; shift 2 ;;
        --prefix)   PREFIX="${2:-}"; BIN_DIR="${PREFIX}/bin"; shift 2 ;;
        --uninstall) UNINSTALL=1; shift ;;
        -h|--help)  usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[ "$(id -u)" = "0" ] || die "请用 root 运行（需要写 systemd 与 /var/run）"

# ---------- 卸载 ----------
if [ "$UNINSTALL" = "1" ]; then
    log "停止服务"
    systemctl stop meow-ext.service 2>/dev/null || true
    # 一定要先把官方入口还回去，否则卸载后飞牛音乐连不上后端。
    if [ -x "${BIN_DIR}/meow-ext" ]; then
        "${BIN_DIR}/meow-ext" restore --config "${CONFIG_FILE}" || true
    fi
    systemctl disable meow-ext.service 2>/dev/null || true
    rm -f "${UNIT}"
    systemctl daemon-reload
    rm -rf "${PREFIX}" "${STATE_DIR}" /var/run/meow-ext.sock
    log "已卸载。配置与数据保留在 ${CONFIG_DIR} 与 ${DATA_DIR}（确认无用后可自行删除）"
    exit 0
fi

# ---------- 安装 ----------
[ -S "${TARGET_SOCKET}" ] || [ -S /var/run/trim_music_upstream.socket ] \
    || die "未检测到飞牛音乐入口 ${TARGET_SOCKET}：请先安装并启动飞牛音乐"

ARCH="$(uname -m)"
case "${ARCH}" in
    aarch64|arm64) GOARCH=arm64 ;;
    x86_64|amd64)  GOARCH=amd64 ;;
    *) die "不支持的架构：${ARCH}" ;;
esac
log "检测到架构 ${ARCH}（${GOARCH}）"

# 二进制优先取本地已编译产物，其次找打包目录里的 fpk 解出来的那份。
SRC_BIN=""
for cand in "${PWD}/dist/bin/meow-ext-${GOARCH}" "${PWD}/meow-ext" "${BIN_DIR}/meow-ext"; do
    if [ -x "${cand}" ]; then SRC_BIN="${cand}"; break; fi
done
[ -n "${SRC_BIN}" ] || die "找不到可执行文件。请先运行 packaging/build.sh，或从 FPK 中取出 app/bin/meow-ext"

mkdir -p "${BIN_DIR}" "${CONFIG_DIR}" "${DATA_DIR}" "${STATE_DIR}"
# 覆盖前先停服务：运行中的二进制被替换会导致进程映像与文件不一致。
systemctl stop meow-ext.service 2>/dev/null || true
install -m 0755 "${SRC_BIN}" "${BIN_DIR}/meow-ext"
log "已安装二进制到 ${BIN_DIR}/meow-ext"

if [ ! -f "${CONFIG_FILE}" ]; then
    case "${MEOW_URL}" in
        http://*|https://*) ;;
        *) MEOW_URL="http://${MEOW_URL}" ;;
    esac
    printf '{\n  "meow_url": "%s"\n}\n' "${MEOW_URL}" > "${CONFIG_FILE}"
    chmod 0600 "${CONFIG_FILE}"
    log "已写入配置 ${CONFIG_FILE}（上游 ${MEOW_URL}）"
else
    log "保留已有配置 ${CONFIG_FILE}"
fi

cat > "${UNIT}" <<EOF
[Unit]
Description=Meow 音源扩展（飞牛音乐入口接管）
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=${PREFIX}
ExecStart=${BIN_DIR}/meow-ext run --config ${CONFIG_FILE}
ExecStopPost=${BIN_DIR}/meow-ext restore --config ${CONFIG_FILE}
Environment=MEOW_EXT_CONFIG=${CONFIG_FILE}
Environment=MEOW_EXT_DATA_DIR=${DATA_DIR}
Environment=MEOW_EXT_STATE_DIR=${STATE_DIR}
Environment=MEOW_EXT_GATEWAY_SOCKET=/var/run/meow-ext.sock
TimeoutStartSec=60
TimeoutStopSec=30
KillMode=mixed
Restart=on-failure
RestartSec=5
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${UNIT}"
systemctl daemon-reload
log "已安装 ${UNIT}"

log "自检"
"${BIN_DIR}/meow-ext" check --config "${CONFIG_FILE}" || die "自检未通过，请按上面的提示处理"

systemctl enable --now meow-ext.service
sleep 1
if systemctl is-active --quiet meow-ext.service; then
    log "已启动。管理页：飞牛桌面 → Meow 音源扩展"
    log "状态：${BIN_DIR}/meow-ext status --config ${CONFIG_FILE}"
else
    journalctl -u meow-ext.service -n 20 --no-pager || true
    die "服务启动失败"
fi
