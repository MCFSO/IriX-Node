#!/bin/sh
# ============================================================
# irix-node 安装脚本（Linux + systemd）
#
# 功能：安装 irix-node 二进制、创建数据目录、写环境配置文件、
#       注册并启动 systemd 服务（开机自启）。
#
# 用法：
#   sudo sh install-systemd.sh                          # 使用默认配置
#   sudo sh install-systemd.sh -bin ./irix-node \
#        -bind 0.0.0.0 -port 23333 -apikey mykey        # 自定义监听与密钥
#
# 参数：
#   -bin <路径>      irix-node 二进制路径（默认脚本同目录 irix-node）
#   -url <URL>       从 URL 下载二进制（覆盖 -bin）
#   -bind <地址>     监听地址（默认 127.0.0.1；局域网访问用 0.0.0.0）
#   -port <端口>     监听端口（默认 12346）
#   -data <目录>     数据目录（默认 /var/lib/irix-node）
#   -apikey <密钥>   固定 API 密钥（留空启用配对码机制，首次启动仅显示一次）
#   -user <用户>     运行用户（默认 root；Bastille/Docker 管理需要高权限）
#   -no-instance-log 关闭实例日志落盘
#   -no-audit-log    关闭审计日志落盘
#   -h               显示帮助
#
# 安装后可随时编辑 /etc/irix-node.env 修改配置，然后
#   systemctl restart irix-node
# ============================================================

set -eu

BIN_SRC="$(dirname "$0")/irix-node"
BIN_URL=""
BIND_ADDR="127.0.0.1"
PORT="12346"
DATA_DIR="/var/lib/irix-node"
APIKEY=""
RUN_USER="root"
INSTANCE_LOG=true
INSTANCE_LOG_MAX=64
AUDIT_LOG=true
AUDIT_LOG_MAX=64

INSTALL_BIN="/usr/local/bin/irix-node"
ENV_FILE="/etc/irix-node.env"
SERVICE_FILE="/etc/systemd/system/irix-node.service"

usage() {
	sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
}

log() {
	echo "[irix-node 安装] $*"
}

die() {
	echo "[错误] $*" >&2
	exit 1
}

# ---- 解析参数 ----
while [ $# -gt 0 ]; do
	case "$1" in
		-bin) BIN_SRC="$2"; shift 2 ;;
		-url) BIN_URL="$2"; shift 2 ;;
		-bind) BIND_ADDR="$2"; shift 2 ;;
		-port) PORT="$2"; shift 2 ;;
		-data) DATA_DIR="$2"; shift 2 ;;
		-apikey) APIKEY="$2"; shift 2 ;;
		-user) RUN_USER="$2"; shift 2 ;;
		-no-instance-log) INSTANCE_LOG=false; shift ;;
		-no-audit-log) AUDIT_LOG=false; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "未知参数: $1（-h 查看帮助）" ;;
	esac
done

# ---- 环境检查 ----
[ "$(id -u)" -eq 0 ] || die "请以 root 运行（sudo sh install-systemd.sh ...）"
command -v systemctl >/dev/null 2>&1 || die "未找到 systemctl（本脚本仅支持 Linux + systemd）"

case "$PORT" in
	''|*[!0-9]*) die "端口无效: $PORT" ;;
esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "端口须在 1-65535: $PORT"

# ---- 准备二进制 ----
if [ -n "$BIN_URL" ]; then
	log "从 URL 下载二进制: $BIN_URL"
	BIN_SRC="$(mktemp /tmp/irix-node.XXXXXX)"
	curl -fL --retry 3 -o "$BIN_SRC" "$BIN_URL" || die "下载失败: $BIN_URL"
fi
[ -f "$BIN_SRC" ] || die "找不到二进制: $BIN_SRC（请先 go build 或用 -bin/-url 指定）"

log "安装二进制 -> $INSTALL_BIN"
install -m 0755 "$BIN_SRC" "$INSTALL_BIN"

# ---- 数据目录 ----
log "创建数据目录: $DATA_DIR"
mkdir -p "$DATA_DIR/logs"
if [ "$RUN_USER" != "root" ]; then
	id "$RUN_USER" >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin -d "$DATA_DIR" "$RUN_USER"
	chown -R "$RUN_USER:$RUN_USER" "$DATA_DIR"
fi

# ---- 环境配置文件 ----
log "写入环境配置: $ENV_FILE"
cat > "$ENV_FILE" <<EOF
# irix-node 配置（修改后 systemctl restart irix-node 生效）
IRIX_BIND=${BIND_ADDR}
IRIX_PORT=${PORT}
IRIX_DATA=${DATA_DIR}
IRIX_APIKEY=${APIKEY}
IRIX_INSTANCE_LOG=${INSTANCE_LOG}
IRIX_INSTANCE_LOG_MAX=${INSTANCE_LOG_MAX}
IRIX_AUDIT_LOG=${AUDIT_LOG}
IRIX_AUDIT_LOG_MAX=${AUDIT_LOG_MAX}
EOF
chmod 600 "$ENV_FILE"   # 可能含 API 密钥，仅 root 可读

# ---- systemd 单元 ----
log "注册 systemd 服务: irix-node.service"
cat > "$SERVICE_FILE" <<'EOF'
[Unit]
Description=IriX Node Daemon
Documentation=https://github.com/MCFSO/IriX-Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=__RUN_USER__
EnvironmentFile=/etc/irix-node.env
ExecStart=/usr/local/bin/irix-node \
    -bind ${IRIX_BIND} \
    -port ${IRIX_PORT} \
    -data ${IRIX_DATA} \
    -apikey ${IRIX_APIKEY} \
    -instance-log=${IRIX_INSTANCE_LOG} \
    -instance-log-max ${IRIX_INSTANCE_LOG_MAX} \
    -audit-log=${IRIX_AUDIT_LOG} \
    -audit-log-max ${IRIX_AUDIT_LOG_MAX}
Restart=on-failure
RestartSec=5
# 节点守护进程管理子进程（游戏服务器/容器），关停时先优雅停止子进程
TimeoutStopSec=60
KillMode=mixed
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
sed -i "s/__RUN_USER__/${RUN_USER}/" "$SERVICE_FILE"

# ---- 启用并启动 ----
log "重载 systemd 并启用服务"
systemctl daemon-reload
systemctl enable irix-node.service >/dev/null 2>&1 || true
systemctl restart irix-node.service

# ---- 完成提示 ----
log "安装完成"
echo ""
echo "  服务状态:  systemctl status irix-node"
echo "  查看日志:  journalctl -u irix-node -f"
echo "  配置文件:  $ENV_FILE"
echo "  数据目录:  $DATA_DIR"
echo "  访问地址:  http://${BIND_ADDR}:${PORT}/api/overview"
if [ -z "$APIKEY" ]; then
	echo "  配对码:    首次启动时在日志中显示（journalctl -u irix-node | grep 配对码），仅此一次，请立即记录"
fi
echo ""