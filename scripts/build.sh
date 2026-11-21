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
echo
du -sh dist/* | awk '{print $2, $1}'
