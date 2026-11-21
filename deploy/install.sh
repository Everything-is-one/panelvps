#!/usr/bin/env bash
# PanelVPS 安装 / 升级脚本（Debian / Ubuntu）
#
# 用法：
#   sudo ./install.sh            安装或升级（保留数据）
#   sudo ./install.sh --uninstall 卸载（保留 /opt/panelvps/data）
#
# 设计原则：幂等、失败即停、任何覆盖前先备份。

set -euo pipefail

PREFIX="/opt/panelvps"
BIN="$PREFIX/bin/panelvps"
CONF="$PREFIX/conf/panel.yaml"
UNIT="/etc/systemd/system/panelvps.service"
DATA="$PREFIX/data"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "[*] $*"; }

[[ $EUID -eq 0 ]] || die "需要 root 权限运行"
command -v systemctl >/dev/null || die "未检测到 systemd，本脚本仅支持 systemd 发行版"

# ---------- 卸载 ----------
if [[ "${1:-}" == "--uninstall" ]]; then
    info "停止并禁用服务"
    systemctl stop panelvps 2>/dev/null || true
    systemctl disable panelvps 2>/dev/null || true
    rm -f "$UNIT"
    systemctl daemon-reload
    info "已移除服务单元。数据目录保留在 $DATA（确认无用后可手动删除）"
    exit 0
fi

# ---------- 定位二进制 ----------
SRC=""
for p in "$(dirname "$0")/bin/panelvps" "$(dirname "$0")/panelvps" "./bin/panelvps"; do
    [[ -f "$p" ]] && { SRC="$p"; break; }
done
[[ -n "$SRC" ]] || die "找不到 panelvps 二进制（请把脚本与 bin/panelvps 放在同一目录）"

# ---------- 环境检查 ----------
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|aarch64) ;;
    *) die "不支持的架构: $ARCH" ;;
esac

if [[ -r /etc/os-release ]]; then
    . /etc/os-release
    info "系统: ${PRETTY_NAME:-unknown}"
fi

# 低内存提示：不阻止安装，但要说清楚
MEM_KB=$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null || echo 0)
if [[ "$MEM_KB" -gt 0 && "$MEM_KB" -lt 900000 ]]; then
    info "检测到内存约 $((MEM_KB/1024))MB，面板自身目标占用 ≤50MB"
fi

# ---------- 安装 ----------
info "创建目录"
mkdir -p "$PREFIX/bin" "$PREFIX/conf" "$DATA"
chmod 700 "$DATA"

if systemctl is-active --quiet panelvps 2>/dev/null; then
    info "停止旧版本"
    systemctl stop panelvps
fi

if [[ -f "$BIN" ]]; then
    info "备份旧版本到 $BIN.prev"
    cp -f "$BIN" "$BIN.prev"
fi

info "安装二进制"
install -m 0755 "$SRC" "$BIN"

if [[ ! -f "$CONF" ]]; then
    info "生成默认配置（面板将只监听 127.0.0.1）"
    "$BIN" info -config "$CONF" >/dev/null 2>&1 || true
fi

info "安装 systemd 单元"
install -m 0644 "$(dirname "$0")/panelvps.service" "$UNIT"
systemctl daemon-reload
systemctl enable panelvps >/dev/null

info "启动服务"
systemctl start panelvps
sleep 2

if ! systemctl is-active --quiet panelvps; then
    echo
    systemctl status panelvps --no-pager -l | tail -20 || true
    die "启动失败，请查看上面的日志"
fi

# 首次安装会生成随机密码，日志里有
PORT=$(grep -E '^listen:' "$CONF" | awk '{print $2}' | awk -F: '{print $2}')
echo
echo "================================================"
echo " PanelVPS 已启动"
echo " 监听: $(grep -E '^listen:' "$CONF" | awk '{print $2}')"
echo " 密码: journalctl -u panelvps | grep 初始管理员"
echo
echo " 默认只监听本机，推荐用 SSH 隧道访问："
echo "   ssh -L ${PORT:-20873}:127.0.0.1:${PORT:-20873} root@<服务器IP>"
echo " 然后浏览器打开 http://127.0.0.1:${PORT:-20873}"
echo "================================================"
echo
