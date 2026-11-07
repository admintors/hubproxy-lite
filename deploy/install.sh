#!/bin/sh
# 在 Alpine (LXC) 里一键安装 / 升级 hubproxy-lite。用法 (root):
#   sh install.sh                         # 优先用同目录/上级 dist 里的二进制; 没有就从 GitHub Release 下载
#   sh install.sh /path/to/binary         # 指定本地二进制
#   HP_REPO=你的用户名/hubproxy-lite sh install.sh   # 指定 Release 来源仓库
#   HP_VERSION=v0.1.0 ...                 # 指定版本, 默认 latest
set -eu

DIR=$(cd "$(dirname "$0")" && pwd)
[ "$(id -u)" -eq 0 ] || { echo "请用 root 运行" >&2; exit 1; }
command -v rc-update >/dev/null 2>&1 || { echo "没找到 OpenRC (rc-update), 这个脚本只适用于 Alpine" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "不支持的架构: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="hubproxy-lite-linux-$ARCH"

# 证书: 访问上游 HTTPS 必需; wget/ssl_client 用于下载
apk add --no-cache ca-certificates >/dev/null

BIN="${1:-}"
if [ -z "$BIN" ]; then
  for c in "$DIR/$ASSET" "$DIR/../dist/$ASSET"; do
    [ -f "$c" ] && BIN="$c" && break
  done
fi

if [ -z "$BIN" ]; then
  [ -n "${HP_REPO:-}" ] || { echo "本地没找到 $ASSET, 且未设置 HP_REPO, 无法下载" >&2; exit 1; }
  V="${HP_VERSION:-latest}"
  if [ "$V" = latest ]; then BASE="https://github.com/$HP_REPO/releases/latest/download"
  else BASE="https://github.com/$HP_REPO/releases/download/$V"; fi
  TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
  echo "从 $BASE 下载..."
  wget -q -O "$TMP/$ASSET" "$BASE/$ASSET" || { echo "下载失败 (这台机器能访问 github.com 吗? 不能的话在别处下载后用 scp 传上来)" >&2; exit 1; }
  wget -q -O "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || { echo "下载校验文件失败" >&2; exit 1; }
  ( cd "$TMP" && grep " $ASSET\$" SHA256SUMS | sha256sum -c - ) || { echo "SHA256 校验不通过, 已中止" >&2; exit 1; }
  BIN="$TMP/$ASSET"
fi

addgroup -S hubproxy 2>/dev/null || true
adduser -S -D -H -s /sbin/nologin -G hubproxy hubproxy 2>/dev/null || true

install -m 0755 "$BIN" /usr/local/bin/hubproxy-lite
install -m 0755 "$DIR/hubproxy-lite.openrc" /etc/init.d/hubproxy-lite
# 已有配置不覆盖, 升级时保留你的设置
[ -f /etc/conf.d/hubproxy-lite ] || install -m 0644 "$DIR/hubproxy-lite.confd" /etc/conf.d/hubproxy-lite

rc-update add hubproxy-lite default >/dev/null
rc-service hubproxy-lite restart || rc-service hubproxy-lite start

sleep 1
if wget -q -O- http://127.0.0.1:5000/healthz 2>/dev/null | grep -q ok; then
  echo "安装完成, 服务运行中 (默认端口 5000)"
else
  echo "已安装, 但健康检查没通过; 看日志: tail /var/log/hubproxy-lite.log"
fi
echo "配置文件: /etc/conf.d/hubproxy-lite  (改完执行 rc-service hubproxy-lite restart)"
echo "★ 记得设置 HP_ALLOW_CIDR, 否则任何能访问该端口的人都能用你的带宽"
