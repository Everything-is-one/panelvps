#!/usr/bin/env bash
# PanelVPS 一键安装引导脚本
#
# 安装最新版（跟随 main 分支脚本 + latest Release 二进制）：
#   curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh | sudo bash
#
# 安装指定版本（脚本与二进制都锁在该 tag，生产环境推荐）：
#   curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/v0.3.2/deploy/bootstrap.sh \
#     | sudo bash -s -- --ref v0.3.2
#
# 自建分发（国内访问 GitHub 慢时用，目录里放 bootstrap.sh + install.sh + 两个 conf + bin/ + SHA256SUMS）：
#   curl -fsSL https://你的域名/panelvps/bootstrap.sh | sudo bash
#
# 只体检不安装（末尾加）：
#   ... | sudo bash -s -- --check
#
# 参数：
#   --base URL     从自有静态目录拉（优先级最高）
#   --repo O/R     GitHub 仓库，默认 Everything-is-one/panelvps
#   --ref REF      分支或 tag，默认 main；写 v* 时二进制锁定到该 tag
#   --token TOKEN  私有仓库的 GitHub token（走 API；不建议在命令行明文传）
#   --check        只体检环境，不安装
#   --public       安装后监听 0.0.0.0，可通过 IP:端口 直接访问
#   --keep         保留临时目录，便于排查
#   -h, --help     显示帮助

set -euo pipefail

REPO="${PV_REPO:-Everything-is-one/panelvps}"
REF="${PV_REF:-main}"
BASE="${PV_BASE:-}"
TOKEN="${GH_TOKEN:-}"
KEEP=0
CHECK_ONLY=0
PUBLIC=0

die() { echo "错误: $*" >&2; exit 1; }
info() { echo "[*] $*"; }
warn() { echo "[!] $*" >&2; }

usage() {
    # 提取文件头连续注释块；curl|bash 时 $0 指向 bash 本身，没有文件可读，退回简短说明
    local src="${BASH_SOURCE[0]:-$0}"
    if [[ -r "$src" ]]; then
        awk 'NR==1 {next} /^#/ {sub(/^# ?/, ""); print; next} {exit}' "$src"
        return
    fi
    cat <<'USAGE'
用法: curl -fsSL <bootstrap.sh 地址> | sudo bash -s -- [选项]

  --base URL     从自有静态目录拉（优先级最高）
  --repo O/R     GitHub 仓库，默认 Everything-is-one/panelvps
  --ref REF      分支或 tag，默认 main；写 v* 时二进制锁定到该 tag
  --check        只体检环境，不安装
  --public       安装后监听 0.0.0.0，可通过 IP:端口 直接访问
  --keep         保留临时目录，便于排查
  -h, --help     显示帮助

完整说明见 https://github.com/Everything-is-one/panelvps/blob/main/deploy/bootstrap.sh
USAGE
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base)   BASE="${2:?--base 需要 URL}"; shift 2 ;;
        --repo)   REPO="${2:?--repo 需要 owner/repo}"; shift 2 ;;
        --ref)    REF="${2:?--ref 需要分支或 tag}"; shift 2 ;;
        --token)  TOKEN="${2:?--token 需要值}"; shift 2 ;;
        --check)  CHECK_ONLY=1; shift ;;
        --public) PUBLIC=1; shift ;;
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
    if curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$url" -o "$out"; then
        return 0
    fi
    # raw 会偶发 TLS 握手失败，退回 contents API 取同一份内容
    warn "raw 拉取失败（$rel），改走 API"
    curl -fsSL --connect-timeout 10 --retry 2 \
        -H "Accept: application/vnd.github.raw" \
        "https://api.github.com/repos/$REPO/contents/$rel?ref=$REF" -o "$out"
}

# Release 资产的直链：指定 tag 时锁定该版本，否则跟随 latest
gh_release_url() {
    if [[ "$REF" == v* ]]; then
        printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$REF" "$1"
    else
        printf 'https://github.com/%s/releases/latest/download/%s' "$REPO" "$1"
    fi
}

# 直链走 github.com，国内网络常被干扰（502 / 超时）；api.github.com 一般可达，
# 公开仓库匿名即可读取资产，所以留一条 API 回退通道。
fetch_via_api() {
    local name="$1" out="$2" api json assets id
    if [[ "$REF" == v* ]]; then
        api="https://api.github.com/repos/$REPO/releases/tags/$REF"
    else
        api="https://api.github.com/repos/$REPO/releases/latest"
    fi
    # API 返回的是带缩进的 pretty JSON，先抹掉空白再做字段定位
    json=$(curl -fsSL --connect-timeout 10 --retry 2 "$api" 2>/dev/null | tr -d ' \n\r\t') || return 1
    [[ -n "$json" ]] || return 1
    # 只保留 assets 数组，避免 release 正文里出现的同名字符串干扰
    assets="${json#*\"assets\":\[}"
    assets="${assets%%\]*}"
    # 按 "{" 把数组切成对象，定位含目标文件名的那一个，再从同段取 id
    id=$(printf '%s' "$assets" | tr '{' '\n' | grep -F "\"name\":\"$name\"" \
        | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
    [[ -n "$id" ]] || return 1
    curl -fsSL --connect-timeout 15 \
        -H "Accept: application/octet-stream" \
        "https://api.github.com/repos/$REPO/releases/assets/$id" -o "$out" \
        || { rm -f "$out"; return 1; }
    [[ -s "$out" ]]
}

# 先直链，失败再走 API
fetch_release_asset() {
    local name="$1" out="$2" url
    url=$(gh_release_url "$name")
    if curl -fsSL --connect-timeout 15 --retry 2 --retry-delay 2 "$url" -o "$out" 2>/dev/null; then
        return 0
    fi
    rm -f "$out"
    warn "直链不通（$url），改走 GitHub API"
    fetch_via_api "$name" "$out"
}

# 二进制的来源：自建分发在 bin/ 下，GitHub 模式只能来自 Release（仓库里不放二进制）
fetch_bin() {
    local out="$TMP/bin/$BIN"
    mkdir -p "$TMP/bin"
    info "下载 $BIN"
    if [[ -n "$BASE" ]]; then
        curl -fsSL --connect-timeout 15 --retry 3 --retry-delay 2 \
            "${BASE%/}/bin/$BIN" -o "$out" \
            || die "下载失败: ${BASE%/}/bin/$BIN"
    elif [[ -n "$TOKEN" ]]; then
        die "私有仓库请用 --base 指定分发地址；Release 资产需要额外鉴权，不做支持"
    else
        fetch_release_asset "$BIN" "$out" \
            || die "下载失败：直链与 API 均不可用。公开仓库需先发布 Release；否则请用 --base 指定自建分发"
    fi
    chmod 0755 "$out"
    # 资产名带架构后缀（便于同一个 SHA256SUMS 覆盖两个架构），
    # 但 install.sh 统一按 bin/panelvps 查找，这里补一份同名副本。
    cp -f "$out" "$TMP/bin/panelvps"
}

# 校验清单也要按来源取：Release 资产不是仓库文件，raw 地址上没有它。
# 取不到不算致命（本地构建 / 自定义分发可能没有），但要明确告诉用户校验被跳过。
fetch_sums() {
    local out="$TMP/SHA256SUMS"
    if [[ -n "$BASE" ]]; then
        curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 \
            "${BASE%/}/SHA256SUMS" -o "$out" || return 1
    elif [[ -n "$TOKEN" ]]; then
        return 1
    else
        fetch_release_asset "SHA256SUMS" "$out" || return 1
    fi
    [[ -s "$out" ]] || { rm -f "$out"; return 1; }
}

info "仓库 $REPO @ $REF，架构 $ARCH"
[[ -n "$BASE" ]] && info "分发地址 $BASE"

fetch "deploy/install.sh" "$TMP/install.sh" \
    || die "拉取 install.sh 失败：${BASE:-$REPO@$REF}"
fetch "deploy/panelvps.service" "$TMP/panelvps.service" \
    || die "拉取 panelvps.service 失败"
fetch "deploy/panelvps-container.conf" "$TMP/panelvps-container.conf" \
    || die "拉取 panelvps-container.conf 失败"
fetch "deploy/panelvps-sysctl.service" "$TMP/panelvps-sysctl.service" \
    || warn "拉取 panelvps-sysctl.service 失败，BBR 开关将不可用"
chmod 0755 "$TMP/install.sh"

# 校验清单可选：有就校验，没有就明确提示
if fetch_sums; then
    info "已取得 SHA256SUMS，安装前会校验二进制"
else
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
    INSTALL_ARGS=()
    [[ "$PUBLIC" -eq 1 ]] && INSTALL_ARGS+=(--public)
    PV_BIN="$TMP/bin/panelvps" bash "$TMP/install.sh" "${INSTALL_ARGS[@]}"
fi
