#!/bin/sh
# 交叉编译静态二进制 (需要 Go >= 1.22)。产物在 dist/
set -eu
cd "$(dirname "$0")"
VER="${VERSION:-0.1.0}"
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
    go build -trimpath -ldflags="-s -w -X main.version=$VER" -o "dist/hubproxy-lite-linux-$arch" .
  echo "built dist/hubproxy-lite-linux-$arch"
done
( cd dist && sha256sum hubproxy-lite-linux-* > SHA256SUMS )
