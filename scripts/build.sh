#!/usr/bin/env bash
# 交叉编译发布产物。用法：
#   ./scripts/build.sh            # 编译 linux/amd64 + linux/arm64
#   VERSION=0.1.0 ./scripts/build.sh

set -euo pipefail
cd "$(dirname "$0")/.."

MODULE="github.com/Everything-is-one/panelvps"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILD_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w \
 -X ${MODULE}/internal/version.Version=${VERSION} \
 -X ${MODULE}/internal/version.Commit=${COMMIT} \
 -X ${MODULE}/internal/version.BuildAt=${BUILD_AT}"

mkdir -p dist
export CGO_ENABLED=0

for target in linux/amd64 linux/arm64; do
    GOOS="${target%/*}"
    GOARCH="${target#*/}"
    out="dist/panelvps-${GOOS}-${GOARCH}"
    echo "[*] building ${out}"
    GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/panelvps
done

echo
ls -lh dist/

# 校验和必须写成「<hash>  <纯文件名>」：早期版本用 `sha256sum bin/xxx` 生成，
# 面板按资产名精确匹配时会找不到；Windows 上的 sha256sum 还会多带一个 * 前缀。
: > dist/SHA256SUMS
for f in dist/panelvps-linux-amd64 dist/panelvps-linux-arm64; do
    [ -f "$f" ] || continue
    sum="$(sha256sum "$f" | cut -d' ' -f1)"
    printf '%s  %s\n' "$sum" "$(basename "$f")" >> dist/SHA256SUMS
done
echo "[*] dist/SHA256SUMS:"
cat dist/SHA256SUMS

echo
du -sh dist/* | awk '{print $2, $1}'
