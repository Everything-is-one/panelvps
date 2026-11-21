#!/usr/bin/env bash
# PanelVPS 一键安装引导脚本
#
# 公开仓库（发布过 Release 之后）：
#   curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh | sudo bash
#
# 自建分发（私有仓库推荐，目录里放本脚本 + install.sh + 两个 conf + bin/ + SHA256SUMS）：
#   curl -fsSL https://你的域名/panelvps/bootstrap.sh | sudo bash
#
# 先体检不安装：
#   curl -fsSL https://.../bootstrap.sh | sudo bash -s -- --check
#
# 参数：
#   --base URL     从自有静态目录拉（优先级最高）
#   --repo O/R     GitHub 仓库，默认 Everything-is-one/panelvps
#   --ref REF      分支或 tag，默认 main
#   --token TOKEN  私有仓库的 GitHub token（走 API；不建议在命令行明文传）
#   --check        只体检环境，不安装
#   --keep         保留临时目录，便于排查
#   -h, --help     显示帮助

set -euo pipefail

REPO="${PV_REPO:-Everything-is-one/panelvps}"
REF="${PV_REF:-main}"
BASE="${PV_BASE:-}"
TOKEN="${GH_TOKEN:-}"
KEEP=0
CHECK_ONLY=0

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "[*] $*"; }
warn() { echo "[!] $*" >&2; }

usage() {
    sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base)   BASE="${2:?--base 需要 URL}"; shift 2 ;;
        --repo)   REPO="${2:?--repo 需要 owner/repo}"; shift 2 ;;
        --ref)    REF="${2:?--ref 需要分支或 tag}"; shift 2 ;;
        --token)  TOKEN="${2:?--token 需要值}"; shift 2 ;;
        --check)  CHECK_ONLY=1; shift ;;
        --keep)   KEEP=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "未知参数: $1（--help 查看用法）" ;;
    esac
done

[[ $EUID -eq 0 ]] || die "需要 root：请用 sudo bash 运行"
command -v curl >/dev/null 2>&1 || die "缺少 curl，请先 apt install -y curl"

ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)  BIN="panelvps-linux-amd64" ;;
    aarch64|arm64) BIN="panelvps-linux-arm64" ;;
    *) die "不支持的架构: $ARCH（只有 amd64 与 arm64）" ;;
esac

TMP=$(mktemp -d)
cleanup() { [[ "$KEEP" -eq 1 ]] || rm -rf "$TMP"; }
trap cleanup EXIT

# fetch <相对路径> <输出文件>；失败时返回非 0，由调用方决定是否致命
fetch() {
    local rel="$1" out="$2" url
    if [[ -n "$BASE" ]]; then
        # 发布包是扁平的（deploy/install.sh 在包里叫 install.sh）
        url="${BASE%/}/${rel#deploy/}"
        curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$url" -o "$out" || return 1
        return 0
    fi
    if [[ -n "$TOKEN" ]]; then
        url="https://api.github.com/repos/$REPO/contents/$rel?ref=$REF"
        curl -fsSL --connect-timeout 10 --retry 2 \
            -H "Authorization: Bearer $TOKEN" \
            -H "Accept: application/vnd.github.raw" \
            "$url" -o "$out" || return 1
        return 0
    fi
    url="https://raw.githubusercontent.com/$REPO/$REF/$rel"
    curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$url" -o "$out" || return 1
}

# 二进制在 GitHub 模式下只能来自 Release，不在仓库里
fetch_bin() {
    local out="$TMP/bin/$BIN" url
    mkdir -p "$TMP/bin"
    if [[ -n "$BASE" ]]; then
        url="${BASE%/}/bin/$BIN"
    elif [[ -n "$TOKEN" ]]; then
        die "私有仓库请用 --base 指定分发地址；GitHub Release 资产需要额外鉴权，不做支持"
    else
        url="https://github.com/$REPO/releases/latest/download/$BIN"
    fi
    info "下载 $BIN"
    curl -fsSL --connect-timeout 15 --retry 3 --retry-delay 2 "$url" -o "$out" \
        || die "下载失败: $url（公开仓库需先发布 Release；否则请用 --base）"
    chmod 0755 "$out"
}

info "仓库 $REPO @ $REF，架构 $ARCH"
[[ -n "$BASE" ]] && info "分发地址 $BASE"

fetch "deploy/install.sh" "$TMP/install.sh" \
    || die "拉取 install.sh 失败：${BASE:-$REPO@$REF}"
fetch "deploy/panelvps.service" "$TMP/panelvps.service" \
    || die "拉取 panelvps.service 失败"
fetch "deploy/panelvps-container.conf" "$TMP/panelvps-container.conf" \
    || die "拉取 panelvps-container.conf 失败"
chmod 0755 "$TMP/install.sh"

# SHA256SUMS 可选：有就校验，没有就明确提示
if fetch "SHA256SUMS" "$TMP/SHA256SUMS"; then
    info "源提供 SHA256SUMS，安装前会校验二进制"
else
    rm -f "$TMP/SHA256SUMS"
    warn "源未提供 SHA256SUMS，跳过二进制校验"
fi

if [[ "$CHECK_ONLY" -eq 0 ]]; then
    fetch_bin
    if [[ -f "$TMP/SHA256SUMS" ]]; then
        # sha256sum 的输出可能是 "  bin/x"（文本模式）或 "*bin/x"（二进制模式），
        # 这里只按文件名匹配，两种都能吃。
        ( cd "$TMP" && grep -E "bin/$BIN\$" SHA256SUMS | sha256sum -c - ) >/dev/null 2>&1 \
            || die "SHA256 校验失败，二进制可能损坏或被篡改"
        info "SHA256 校验通过"
    fi
fi

echo
if [[ "$CHECK_ONLY" -eq 1 ]]; then
    bash "$TMP/install.sh" --check
else
    bash "$TMP/install.sh"
fi
