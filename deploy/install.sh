#!/usr/bin/env bash
# PanelVPS 安装 / 升级脚本（Debian / Ubuntu）
#
# 用法：
#   sudo ./install.sh            安装或升级（保留数据）
#   sudo ./install.sh --check    只体检环境，不安装
#   sudo ./install.sh --uninstall 卸载（保留 /opt/panelvps/data）
#
# 设计原则：幂等、失败即停、任何覆盖前先备份。
# 容器（LXC / Docker 内的 systemd）会自动安装降级覆盖，见 panelvps-container.conf。

set -euo pipefail

PREFIX="/opt/panelvps"
BIN="$PREFIX/bin/panelvps"
CONF="$PREFIX/conf/panel.yaml"
UNIT="/etc/systemd/system/panelvps.service"
DROPIN_DIR="/etc/systemd/system/panelvps.service.d"
DATA="$PREFIX/data"

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "[*] $*"; }
ok() { echo "  [ok]   $*"; }
warn() { echo "  [注意] $*"; }
bad() { echo "  [不可用] $*"; }

# ---------- 探测函数 ----------

# cap_has <位号>：判断当前进程有效能力位。
# CAP_NET_BIND_SERVICE=10, CAP_NET_ADMIN=12, CAP_NET_RAW=13
cap_has() {
    local hex bits
    hex=$(awk '/^CapEff:/{print $2; exit}' /proc/self/status 2>/dev/null || true)
    [[ -n "$hex" ]] || return 1
    hex=${hex#0x}
    case "$hex" in *[!0-9a-fA-F]*) return 1 ;; esac
    bits=$((16#$hex))
    (( (bits >> $1) & 1 ))
}

detect_container() {
    local v=""
    if command -v systemd-detect-virt >/dev/null 2>&1; then
        v=$(systemd-detect-virt --container 2>/dev/null || true)
        if [[ "$v" == "none" ]]; then v=""; fi
    fi
    if [[ -z "$v" && -r /run/systemd/container ]]; then
        v=$(tr -d '\n' < /run/systemd/container 2>/dev/null || true)
    fi
    printf '%s' "$v"
}

# cgroup 里的真实内存限额。LXC 中 /proc/meminfo 常常是宿主的值。
mem_limit_mb() {
    local raw=""
    if [[ -r /sys/fs/cgroup/memory.max ]]; then
        raw=$(head -1 /sys/fs/cgroup/memory.max 2>/dev/null || true)
    elif [[ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]]; then
        raw=$(head -1 /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || true)
    fi
    case "$raw" in ''|max|*[!0-9]*) return 1 ;; esac
    # 未设限额时 cgroup 会给出一个天文数字
    (( raw > 0 && raw < 1099511627776 )) || return 1
    echo $(( raw / 1048576 ))
}

firewall_backend() {
    if command -v nft >/dev/null 2>&1 && nft add table inet pv_probe >/dev/null 2>&1; then
        nft delete table inet pv_probe >/dev/null 2>&1 || true
        echo "nftables"
    elif command -v iptables-legacy >/dev/null 2>&1 && iptables-legacy -L -n >/dev/null 2>&1; then
        echo "iptables-legacy"
    else
        echo "none"
    fi
}

env_report() {
    local arch container memkb limit fw
    arch=$(uname -m)
    container=$(detect_container)

    if [[ -r /etc/os-release ]]; then
        . /etc/os-release
        info "系统: ${PRETTY_NAME:-unknown} ($arch)"
    else
        info "系统: unknown ($arch)"
    fi

    memkb=$(awk '/MemTotal/{print $2; exit}' /proc/meminfo 2>/dev/null || true)
    memkb=${memkb:-0}
    limit=$(mem_limit_mb || true)
    if [[ -n "$limit" ]]; then
        info "内存: /proc/meminfo 报 $((memkb/1024))MB，cgroup 限额 ${limit}MB"
        (( limit >= 400 )) || warn "cgroup 限额低于 400MB，面板 + Xray 会很紧张"
    else
        info "内存: $((memkb/1024))MB"
    fi

    if [[ -n "$container" ]]; then
        info "运行环境: 容器 ($container) —— 会安装容器专用 systemd 覆盖"
    else
        info "运行环境: 物理机 / KVM"
    fi

    echo "  能力与功能："
    if cap_has 12; then
        ok "CAP_NET_ADMIN —— 防火墙规则可下发、可封禁"
    else
        bad "CAP_NET_ADMIN —— 防火墙只能查看，无法下发（非特权容器的正常现象）"
    fi

    # 绑 80/443 有两条路：拿到 CAP_NET_BIND_SERVICE，或内核放开非特权端口
    local port_start=""
    if [[ -r /proc/sys/net/ipv4/ip_unprivileged_port_start ]]; then
        port_start=$(cat /proc/sys/net/ipv4/ip_unprivileged_port_start 2>/dev/null || true)
    fi
    if cap_has 10; then
        ok "CAP_NET_BIND_SERVICE —— 可直接监听 80 / 443"
    elif [[ "$port_start" == "0" ]]; then
        ok "ip_unprivileged_port_start=0 —— 非特权进程也能监听 80 / 443"
    else
        warn "无法绑定 1024 以下端口（缺 CAP_NET_BIND_SERVICE，且 ip_unprivileged_port_start=${port_start:-未知}）"
        warn "容器里需宿主设置 net.ipv4.ip_unprivileged_port_start=0，或让 Nginx 听高端口由宿主反代"
    fi

    fw=$(firewall_backend)
    if [[ "$fw" == "none" ]]; then
        bad "防火墙后端: 无（nft 与 iptables-legacy 都不可用）"
    else
        ok "防火墙后端: $fw"
    fi

    if [[ -d /dev/pts && -e /dev/ptmx ]]; then
        ok "PTY 可用 —— Web 终端可连接"
    else
        bad "PTY 不可用 —— Web 终端会连不上（容器需挂载 devpts）"
    fi

    if command -v journalctl >/dev/null 2>&1; then
        ok "journald 可用 —— 日志页与初始密码可查"
    else
        warn "无 journalctl —— 日志页只能读文件，初始密码需看前台输出"
    fi

    local entry bin why
    for entry in "nginx|站点配置下发" "certbot|申请证书" "xray|代理功能"; do
        bin="${entry%%|*}"
        why="${entry#*|}"
        if command -v "$bin" >/dev/null 2>&1; then
            ok "$bin: $(command -v "$bin")"
        elif [[ "$bin" == "xray" && -x /usr/local/bin/xray ]]; then
            ok "xray: /usr/local/bin/xray"
        else
            warn "$bin 未安装 —— ${why}暂不可用"
        fi
    done

    local port=20873
    if [[ -r "$CONF" ]]; then
        port=$(grep -E '^listen:' "$CONF" | awk '{print $2}' | awk -F: '{print $2}' || true)
        port=${port:-20873}
    fi
    if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":${port} "; then
        warn "端口 ${port} 已被占用，安装前请先停掉占用进程"
    fi
}

# ---------- 参数 ----------
ACTION="${1:-install}"
[[ $EUID -eq 0 ]] || die "需要 root 权限运行"
command -v systemctl >/dev/null || die "未检测到 systemd。若在容器里，请先用 systemd 作为 init 启动容器"

if [[ "$ACTION" == "--check" || "$ACTION" == "check" ]]; then
    env_report
    echo
    echo "体检完成，未做任何修改。"
    exit 0
fi

# ---------- 卸载 ----------
if [[ "$ACTION" == "--uninstall" ]]; then
    info "停止并禁用服务"
    systemctl stop panelvps 2>/dev/null || true
    systemctl disable panelvps 2>/dev/null || true
    rm -f "$UNIT"
    rm -rf "$DROPIN_DIR"
    systemctl daemon-reload
    info "已移除服务单元。数据目录保留在 $DATA（确认无用后可手动删除）"
    exit 0
fi

# ---------- 定位二进制 ----------
SRC=""
for p in "$(dirname "$0")/bin/panelvps" "$(dirname "$0")/panelvps" "./bin/panelvps" "/opt/panelvps/bin/panelvps.new"; do
    if [[ -f "$p" ]]; then SRC="$p"; break; fi
done
[[ -n "$SRC" ]] || die "找不到 panelvps 二进制（请把脚本与 bin/panelvps 放在同一目录）"

# ---------- 环境检查 ----------
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|aarch64) ;;
    *) die "不支持的架构: $ARCH" ;;
esac

CONTAINER=$(detect_container)

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

# 容器里主单元的加固项会让 systemd 直接拒绝启动，提前换成降级版
if [[ -n "$CONTAINER" ]]; then
    info "检测到容器环境（$CONTAINER），安装容器专用覆盖"
    mkdir -p "$DROPIN_DIR"
    install -m 0644 "$(dirname "$0")/panelvps-container.conf" "$DROPIN_DIR/container.conf"
else
    # 从容器迁到真机时，别把旧的降级覆盖留着
    if [[ -d "$DROPIN_DIR" ]]; then
        info "非容器环境，移除遗留的容器覆盖"
        rm -rf "$DROPIN_DIR"
    fi
fi

systemctl daemon-reload
systemctl enable panelvps >/dev/null

info "启动服务"
systemctl start panelvps
sleep 2

if ! systemctl is-active --quiet panelvps; then
    echo
    echo "---------------- systemd 状态 ----------------"
    systemctl status panelvps --no-pager -l | tail -25 || true
    echo "----------------------------------------------"
    echo
    warn "常见原因（按概率排序）："
    warn "1) 容器内 systemd 不接受 MemoryMax / ProtectSystem —— 本脚本已自动装覆盖，若仍失败请检查 $DROPIN_DIR/container.conf 是否生效"
    warn "2) 配置里 listen 的端口已被占用"
    warn "3) 数据目录权限不对：chown -R root:root $DATA && chmod 700 $DATA"
    warn "4) 磁盘满：df -h $PREFIX"
    die "启动失败，请查看上面的日志"
fi

# 网站与证书依赖外部组件，缺了不影响面板启动，但要在安装时就说明白
for pair in "nginx:/etc/nginx/conf.d" "certbot:/etc/letsencrypt" "xray:/usr/local/bin/xray"; do
    bin="${pair%%:*}"
    if ! command -v "$bin" >/dev/null 2>&1; then
        info "未检测到 $bin —— 相关功能需要它，可在面板「商店」里安装，或按官方文档安装"
    fi
done
if [[ ! -d /etc/nginx/conf.d ]]; then
    info "提示：/etc/nginx/conf.d 不存在时，站点下发会失败；装好 Nginx 后该目录会自动创建"
fi

PORT=$(grep -E '^listen:' "$CONF" | awk '{print $2}' | awk -F: '{print $2}' || true)
PORT=${PORT:-20873}
echo
echo "================================================"
echo " PanelVPS 已启动"
echo " 监听: $(grep -E '^listen:' "$CONF" | awk '{print $2}' || echo '127.0.0.1:20873')"
echo " 密码: journalctl -u panelvps | grep 初始管理员"
echo
echo " 默认只监听本机，推荐用 SSH 隧道访问："
echo "   ssh -L ${PORT}:127.0.0.1:${PORT} root@<服务器IP>"
echo " 然后浏览器打开 http://127.0.0.1:${PORT}"
echo "================================================"
echo

if [[ -n "$CONTAINER" ]]; then
    echo "容器环境提醒："
    echo "  · 面板只监听 127.0.0.1，必须走上面的 SSH 隧道；要在局域网直连就把 conf 里 listen 改成 0.0.0.0:${PORT}"
    echo "  · 防火墙是否可下发取决于容器有没有 CAP_NET_ADMIN，可跑 ./install.sh --check 复核"
    echo "  · 非特权容器绑不了 80/443，Nginx 站点需宿主放开 net.ipv4.ip_unprivileged_port_start=0"
    echo
fi
