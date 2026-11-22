#!/usr/bin/env bash
# PanelVPS 安装 / 升级脚本（Debian / Ubuntu）
#
# 用法：
#   sudo ./install.sh            安装或升级（保留数据）
#   sudo ./install.sh --check    只体检环境，不安装
#   sudo ./install.sh --public   监听 0.0.0.0，可通过 IP:端口 直接访问
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

    # 配置能不能解析，是「装完起不来」里最常见也最难自查的一种：
    # 报错只有一行 yaml: line N: ...，不说 N 是哪一行。
    if [[ -x "$BIN" && -r "$CONF" ]]; then
        local crc=0
        "$BIN" check -config "$CONF" >/dev/null 2>&1 || crc=$?
        case "$crc" in
            0)  ok "配置文件可解析: $CONF" ;;
            2)  : ;;   # 老版本二进制没有 check 子命令
            *)  bad "配置文件解析失败，服务会启动即退出："
                bad "  看细节 panelvps check -config $CONF ；能自动修就加 -fix" ;;
        esac
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

    echo "  systemd 沙箱："
    if [[ -f "$UNIT" ]]; then
        if grep -qE "^ProtectSystem=no$" "$UNIT" && grep -qE "^ReadOnlyPaths=.*-/boot" "$UNIT"; then
            ok "单元默认放开写权限，仅 /boot /efi 只读（装包、建站、防火墙都不会再撞沙箱）"
        elif grep -qE "^ProtectSystem=(strict|full|yes|true)$" "$UNIT"; then
            PS_VAL="$(grep -m1 -E '^ProtectSystem=' "$UNIT")"
            if grep -qE "^ProtectSystem=strict$" "$UNIT"; then
                warn "单元仍是旧写法 $PS_VAL：整机只读，装包/建站全部会失败。"
            else
                warn "单元仍是旧写法 $PS_VAL：系统目录只读，装包会报 read-only file system。"
            fi
            warn "重跑安装脚本，或到「设置 → 服务单元」用内置版本覆盖后重启面板。"
        else
            warn "单元的写保护写法无法识别，装包/建站可能写不进系统目录"
        fi

        # apt 的 sandbox 用 _apt 下载，root 对 _apt 名下的文件改权限要靠 FOWNER。
        # 少了它不致命（apt 只打 warning），但文件管理器改权限、站点 chown 会失败。
        if grep -q "CapabilityBoundingSet=.*CAP_FOWNER" "$UNIT"; then
            ok "能力集含 CAP_FOWNER（apt 与改权限不会报 Operation not permitted）"
        elif grep -q "CapabilityBoundingSet=" "$UNIT"; then
            warn "单元的 CapabilityBoundingSet 没有 CAP_FOWNER："
            warn "  apt 会打一串「chmod ... failed - Operation not permitted」warning，"
            warn "  文件管理器改权限也会失败。重跑安装脚本，或到「设置 → 服务单元」覆盖后重启。"
        fi

        # 上面 cap_has 查的是「当前这个 shell」能不能绑低端口，用的是 SSH 会话
        # 的完整能力集，看不出单元本身有没有放行。两者的差别正是装 Nginx 失败的
        # 原因：nginx -t 由面板拉起、继承单元的边界集，缺 BIND_SERVICE 就 EACCES。
        if grep -q "CapabilityBoundingSet=.*CAP_NET_BIND_SERVICE" "$UNIT"; then
            ok "能力集含 CAP_NET_BIND_SERVICE（校验 nginx -t、中转低端口可用）"
        elif grep -q "CapabilityBoundingSet=" "$UNIT"; then
            warn "单元的 CapabilityBoundingSet 没有 CAP_NET_BIND_SERVICE："
            warn "  软件商店装 Nginx 会在最后一步 nginx -t 报 bind() to 0.0.0.0:80"
            warn "  failed (13: Permission denied)，随后整单回滚。"
            warn "  重跑安装脚本，或到「设置 → 服务单元」覆盖后重启面板。"
        fi
    else
        warn "未找到服务单元：$UNIT"
    fi

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
ACTION="install"
PUBLIC=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --check|check)  ACTION="--check"; shift ;;
        --uninstall)    ACTION="--uninstall"; shift ;;
        --public)       PUBLIC=1; shift ;;
        install)        ACTION="install"; shift ;;
        -h|--help)      sed -n '2,12p' "$0"; exit 0 ;;
        *)              die "未知参数: $1（--help 查看用法）" ;;
    esac
done

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
    systemctl stop panelvps-sysctl 2>/dev/null || true
    systemctl disable panelvps-sysctl 2>/dev/null || true
    rm -f "$UNIT"
    rm -f /etc/systemd/system/panelvps-sysctl.service
    if [[ -L /usr/local/bin/panelvps ]]; then
        rm -f /usr/local/bin/panelvps
    fi
    rm -rf "$DROPIN_DIR"
    if [[ -f /etc/sysctl.d/99-panelvps.conf ]]; then
        rm -f /etc/sysctl.d/99-panelvps.conf
        if [[ "$(cat /proc/sys/net/ipv4/tcp_congestion_control 2>/dev/null || true)" == "bbr" ]]; then
            sysctl -w net.ipv4.tcp_congestion_control=cubic >/dev/null 2>&1 || true
        fi
    fi
    systemctl daemon-reload
    info "已移除服务单元。数据目录保留在 $DATA（确认无用后可手动删除）"
    exit 0
fi

# ---------- 环境检查 ----------
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)  SUFFIX="amd64" ;;
    aarch64|arm64) SUFFIX="arm64" ;;
    *) die "不支持的架构: $ARCH" ;;
esac

# ---------- 定位二进制 ----------
# bootstrap.sh 会通过 PV_BIN 指过来；手工执行时按约定目录查找。
# 发布包里的资产名带架构后缀，仓库构建则多在 bin/ 或 dist/。
SRC="${PV_BIN:-}"
if [[ -z "$SRC" ]]; then
    for p in \
        "$(dirname "$0")/bin/panelvps" \
        "$(dirname "$0")/bin/panelvps-linux-$SUFFIX" \
        "$(dirname "$0")/panelvps" \
        "./bin/panelvps" \
        "./dist/panelvps-linux-$SUFFIX" \
        "./dist/panelvps" \
        "/opt/panelvps/bin/panelvps.new"; do
        if [[ -f "$p" ]]; then SRC="$p"; break; fi
    done
fi
[[ -n "$SRC" ]] || die "找不到 panelvps 二进制：请用 PV_BIN=/path/to/panelvps 指定，或把脚本与 bin/panelvps 放在同一目录"
[[ -f "$SRC" ]] || die "PV_BIN 指向的二进制不存在: $SRC"

CONTAINER=$(detect_container)

# ---------- 安装 ----------
info "创建目录"
mkdir -p "$PREFIX/bin" "$PREFIX/conf" "$DATA"
chmod 700 "$DATA"

# 面板要写的系统目录不一定存在（比如没装 nginx 时没有 /etc/nginx/conf.d）。
# 提前建出来：一来写操作不会因为缺目录失败，二来以后装了 nginx 也不必修
# 目录再重启面板。
info "准备面板常用目录"
mkdir -p /usr/local/share /etc/nftables.d /etc/sysctl.d /etc/systemd/system \
         /etc/nginx/conf.d /etc/xray /etc/letsencrypt /var/lib/letsencrypt /var/log/letsencrypt

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

# 让用户在任何目录敲 panelvps 都能进管理菜单（systemd 用的是绝对路径，不受影响）
LINK="/usr/local/bin/panelvps"
if [[ -d /usr/local/bin ]]; then
    ln -sf "$BIN" "$LINK"
    ok "命令已链接到 $LINK"
fi

NEW_INSTALL=0
if [[ ! -f "$CONF" ]]; then
    NEW_INSTALL=1
    info "生成默认配置（面板将只监听 127.0.0.1）"
    "$BIN" info -config "$CONF" >/dev/null 2>&1 || true
fi

# v0.3.0 改端口时会把 data_dir 的相对路径按执行目录固化成绝对路径，
# 典型的残留是 /root/data 或 /data。升级上来的人需要知道这件事，
# 否则服务仍然起不来，而且报错只有一行 exit 1。
DATA_DIR_CFG=$(sed -n 's/^data_dir:[[:space:]]*//p' "$CONF" | head -1 | tr -d '"' || true)
if [[ "$DATA_DIR_CFG" == /* && "$DATA_DIR_CFG" != "$DATA" ]]; then
    warn "配置里的 data_dir 是 $DATA_DIR_CFG，不是安装目录 $DATA"
    warn "如果这是改端口后出现的，数据其实还在 $DATA，执行："
    warn "  sed -i 's|^data_dir:.*|data_dir: ./data|' $CONF && systemctl restart panelvps"
fi

# 同一次污染还会把 backup_dir 写成 /root/data/backups 的绝对路径。
# 用户只改了 data_dir 的话，备份仍指向旧目录，在服务沙箱里是只读的，
# 一点「立即备份」就报 read-only file system。
BACKUP_DIR_CFG=$(sed -n 's/^backup_dir:[[:space:]]*//p' "$CONF" | head -1 | tr -d '"' || true)
if [[ "$BACKUP_DIR_CFG" == /* && "$BACKUP_DIR_CFG" != "$DATA/backups" ]]; then
    warn "配置里的 backup_dir 是 $BACKUP_DIR_CFG，建议改回相对值："
    warn "  sed -i 's|^backup_dir:.*|backup_dir: backups|' $CONF && systemctl restart panelvps"
fi

if [[ "$PUBLIC" -eq 1 ]]; then
    CFG_PORT=$(grep -E '^listen:' "$CONF" | sed 's/.*://' | tr -d ' ' || true)
    CFG_PORT=${CFG_PORT:-20873}
    sed -i -E "s|^listen:.*|listen: 0.0.0.0:${CFG_PORT}|" "$CONF"
    info "监听地址已改为 0.0.0.0:${CFG_PORT}（可通过 IP:端口 访问）"
fi

info "安装 systemd 单元"
# 单元已改成「默认可写，仅 /boot /efi 只读」，不再靠 ReadWritePaths 白名单开洞，
# 所以下面这几个目录不是沙箱前提，只是软件商店的落地点（download 动作自己也会建）。
mkdir -p /usr/local/bin /usr/local/share /etc/xray
install -m 0644 "$(dirname "$0")/panelvps.service" "$UNIT"

if [[ -f "$(dirname "$0")/panelvps-sysctl.service" ]]; then
    install -m 0644 "$(dirname "$0")/panelvps-sysctl.service" /etc/systemd/system/panelvps-sysctl.service
else
    warn "未找到 panelvps-sysctl.service，BBR 开关将不可用"
fi

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
systemctl enable panelvps-sysctl >/dev/null 2>&1 || true

info "检查配置"
# 配置坏了的时候服务会无限重启（单元里是 Restart=always），systemd 状态里
# 只有 status=1/FAILURE，journald 里只有一行「yaml: line N: ...」。所以启动
# 之前先解析一遍：能修就修（已知形态，改前备份），修不了就带着出错行停手。
CHECK_OUT=$(mktemp)
CHECK_RC=0
"$BIN" check -config "$CONF" >"$CHECK_OUT" 2>&1 || CHECK_RC=$?
if [[ "$CHECK_RC" -eq 2 ]]; then
    # 老版本二进制没有 check 子命令，不能因此挡住安装
    info "当前二进制不支持 check，跳过配置预检"
    CHECK_RC=0
elif [[ "$CHECK_RC" -ne 0 ]]; then
    sed 's/^/  /' "$CHECK_OUT" >&2
    warn "配置解析失败，尝试按已知形态修复"
    "$BIN" check -config "$CONF" -fix 2>&1 | sed 's/^/  /' || true
    CHECK_RC=0
    "$BIN" check -config "$CONF" >"$CHECK_OUT" 2>&1 || CHECK_RC=$?
fi
if [[ "$CHECK_RC" -ne 0 ]]; then
    echo
    sed 's/^/  /' "$CHECK_OUT" >&2
    echo
    rm -f "$CHECK_OUT"
    warn "上面标 → 的那一行就是要改的地方；改好 $CONF 后重跑本脚本。"
    die "配置预检未通过，未启动服务（避免 systemd 无限重启刷日志）"
fi
rm -f "$CHECK_OUT"

info "启动服务"
systemctl start panelvps

# 等端口真的能连上，而不是只看 systemd 状态。
# Type=simple 的单元在进程 fork 之后立刻就是 active，端口 bind 失败要再过一瞬
# 才退出；只 sleep 2 再查 is-active，赶上快的机器会以为启动成功了。
H_ADDR=$(sed -n 's/^listen:[[:space:]]*//p' "$CONF" | head -1 | tr -d ' "' || true)
H_HOST=${H_ADDR%:*}
H_PORT=${H_ADDR##*:}
case "$H_HOST" in
    ""|"0.0.0.0"|"::"|"[::]") H_HOST=127.0.0.1 ;;
esac
[[ "$H_PORT" =~ ^[0-9]+$ ]] || H_PORT=20873

probe_panel() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS -m 1 -o /dev/null "http://$1:$2/healthz" 2>/dev/null
    else
        (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null
    fi
}

info "等待面板响应（$H_HOST:$H_PORT）"
READY=0
i=0
while [[ $i -lt 20 ]]; do
    systemctl is-active --quiet panelvps || break
    # 绑定在具体网卡 IP 上时，回环不一定通，两个都试
    if probe_panel "$H_HOST" "$H_PORT" || probe_panel 127.0.0.1 "$H_PORT"; then
        READY=1
        break
    fi
    sleep 0.5
    i=$((i + 1))
done

if [[ "$READY" -ne 1 ]]; then
    echo
    echo "---------------- systemd 状态 ----------------"
    systemctl status panelvps --no-pager -l | tail -25 || true
    echo "---------------- 最近日志 --------------------"
    journalctl -u panelvps -n 30 --no-pager 2>/dev/null | tail -30 || true
    echo "----------------------------------------------"
    echo
    warn "常见原因（按概率排序）："
    warn "1) 配置里 listen 的端口已被占用：ss -ltnp | grep :$H_PORT"
    warn "2) 配置里 listen 的地址本机没有（IP 写错或网卡还没起来）：$H_ADDR"
    warn "3) 容器内 systemd 不接受 MemoryMax / ProtectSystem —— 本脚本已自动装覆盖，若仍失败请检查 $DROPIN_DIR/container.conf 是否生效"
    warn "4) 数据目录权限不对：chown -R root:root $DATA && chmod 700 $DATA"
    warn "5) 磁盘满：df -h $PREFIX"
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

# 新装时把初始密码从 journald 里取出来直接显示，省掉手工 grep。
# 优先解析 slog 的 JSON 行（"password":"..."），其次是启动横幅里的「密  码: ...」。
INIT_PWD=""
if [[ "$NEW_INSTALL" == "1" ]] && command -v journalctl >/dev/null 2>&1; then
    # journald 落盘有延迟，最多等 5 秒
    for _ in 1 2 3 4 5; do
        LOG=$(journalctl -u panelvps --no-pager -n 200 2>/dev/null || true)
        INIT_PWD=$(printf '%s\n' "$LOG" | sed -n 's/.*"password":"\([^"]*\)".*/\1/p' | tail -1)
        if [[ -z "$INIT_PWD" ]]; then
            INIT_PWD=$(printf '%s\n' "$LOG" | sed -n 's/^[[:space:]]*密[[:space:]]*码:[[:space:]]*//p' | tail -1)
        fi
        if [[ -n "$INIT_PWD" ]]; then break; fi
        sleep 1
    done
fi

LISTEN_ADDR=$(grep -E '^listen:' "$CONF" | awk '{print $2}' || true)
LISTEN_ADDR=${LISTEN_ADDR:-127.0.0.1:20873}
PORT=${LISTEN_ADDR##*:}

echo
echo "================================================"
echo " PanelVPS 已启动"
echo " 监听: ${LISTEN_ADDR}"
if [[ -n "$INIT_PWD" ]]; then
    echo " 用户名: admin"
    echo " 密  码: $INIT_PWD"
else
    echo " 密码: journalctl -u panelvps --no-pager | grep password"
    echo "       （已装过则沿用原密码；忘了可 panelvps reset-password）"
fi
echo
case "$LISTEN_ADDR" in
    0.0.0.0:*|\[::\]:*|:::*)
        ACCESS_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
        echo " 访问: http://${ACCESS_IP:-<服务器公网IP>}:${PORT}"
        echo " 注意: 云厂商安全组需放行 ${PORT}/tcp；面板是 HTTP，请使用强密码。"
        ;;
    *)
        echo " 仅监听本机，建议用 SSH 隧道访问："
        echo "   ssh -L ${PORT}:127.0.0.1:${PORT} root@<服务器IP>"
        echo " 然后浏览器打开 http://127.0.0.1:${PORT}"
        ;;
esac
echo " 管理: 输入 panelvps 回车，打开管理菜单（启停 / 端口 / 重置密码 / BBR）"
echo "================================================"
echo

if [[ -n "$CONTAINER" ]]; then
    echo "容器环境提醒："
    case "$LISTEN_ADDR" in
        127.0.0.1:*|\[::1\]:*)
            echo "  · 面板只监听本机，必须走上面的 SSH 隧道；要直连就把 conf 里 listen 改成 0.0.0.0:${PORT}"
            ;;
        *)
            echo "  · 面板已监听 ${LISTEN_ADDR}；NAT 容器还需要商家面板做端口映射"
            ;;
    esac
    echo "  · 防火墙是否可下发取决于容器有没有 CAP_NET_ADMIN，可跑 ./install.sh --check 复核"
    echo "  · 非特权容器绑不了 80/443，Nginx 站点需宿主放开 net.ipv4.ip_unprivileged_port_start=0"
    echo
fi
