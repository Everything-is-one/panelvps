#!/usr/bin/env bash
# 把 PanelVPS 安装包发布到一个静态目录（在本机运行）。
#
# 用法：
#   ./scripts/publish.sh root@1.2.3.4:/www/wwwroot/dl.example.com/panelvps
#   ./scripts/publish.sh root@host:/path --base https://dl.example.com/panelvps
#
# 做的事：交叉编译 → 组装安装包 → 生成 SHA256SUMS → 打包上传。
# 目标目录需要能被 HTTP 匿名访问，小鸡才能 curl 到。

set -euo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:-}"
if [[ -z "$TARGET" ]]; then
    echo "用法: $0 user@host:/path [--base https://你的域名/panelvps]" >&2
    exit 2
fi
shift

BASE_URL=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --base) BASE_URL="${2:?--base 需要 URL}"; shift 2 ;;
        *) echo "未知参数: $1" >&2; exit 2 ;;
    esac
done

if [[ "$TARGET" != *:* ]]; then
    echo "目标格式应为 user@host:/path，例如 root@1.2.3.4:/www/wwwroot/dl.example.com/panelvps" >&2
    exit 2
fi
HOST="${TARGET%%:*}"
DIR="${TARGET#*:}"

echo "[*] 交叉编译"
./scripts/build.sh

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/bin"
install -m 0755 deploy/bootstrap.sh deploy/install.sh "$STAGE/"
install -m 0644 deploy/panelvps.service deploy/panelvps-container.conf "$STAGE/"
cp dist/panelvps-linux-amd64 dist/panelvps-linux-arm64 "$STAGE/bin/"
chmod 0755 "$STAGE"/bin/*

echo "[*] 生成 SHA256SUMS"
(cd "$STAGE" && sha256sum bin/* > SHA256SUMS)

echo "[*] 上传到 $HOST:$DIR"
ssh "$HOST" "mkdir -p '$DIR'"
tar czf - -C "$STAGE" . | ssh "$HOST" "tar xzf - -C '$DIR'"

echo
echo "发布完成，包含："
(cd "$STAGE" && find . -type f | sort | sed 's|^\./|  |')
echo
echo "小鸡上执行："
if [[ -n "$BASE_URL" ]]; then
    echo "  curl -fsSL ${BASE_URL%/}/bootstrap.sh | sudo bash"
    echo "  curl -fsSL ${BASE_URL%/}/bootstrap.sh | sudo bash -s -- --check   # 先体检"
else
    echo "  curl -fsSL https://<你的域名>/panelvps/bootstrap.sh | sudo bash"
fi
