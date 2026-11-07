#!/bin/sh
# hubproxy-lite 一键安装 / 升级 / 卸载 (Alpine + OpenRC)。所有设置都用命令行参数传入, 装完即用。
#
# 最常用 (root):
#   wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | \
#     sh -s -- --port 8080 --allow 203.0.113.7
#
# 用 sh install.sh --help 查看全部参数。
set -eu

REPO_DEFAULT="admintors/hubproxy-lite"

usage() {
cat <<'EOF'
用法: sh install.sh [参数]

必填其一:
  --allow  IP或网段    只允许这些来源使用, 逗号分隔。例: 203.0.113.7,192.168.0.0/16
  --open               明确表示不做来源限制 (任何人都能用你的流量, 不推荐)

常用:
  --port N             监听端口, 默认 5000, 必须 >= 1024 (服务不以 root 运行)
  --listen IP          监听地址, 默认 0.0.0.0
  --public-url URL     对外地址, 例 http://203.0.113.10:8080 (不填则按请求 Host 自动推断)
  --max-streams N      同时下载数上限, 默认 6 (0.1 核机器建议 4~8)

可选:
  --rate-limit N       每个 IP 在 --rate-hours 内最多请求数, 默认 500
  --rate-hours H       限流窗口(小时), 默认 3
  --max-file-mb N      GitHub 单文件大小上限(MB), 默认 2048
  --image-allow  列表  镜像白名单, 逗号分隔, 支持 * 结尾通配
  --image-deny   列表  镜像黑名单
  --registries   列表  额外 registry, 格式 前缀=上游主机, 逗号分隔
  --upstream-proxy URL 出站代理, socks5://host:port 或 http://host:port
  --trust-proxy        前面还有反向代理时才加, 信任 X-Forwarded-For
  --log-access         记录每个请求 (增加磁盘写入)

安装来源:
  --bin PATH           使用本地二进制
  --repo 用户/仓库     Release 来源, 默认 admintors/hubproxy-lite
  --tag  vX.Y.Z        指定版本, 默认 latest

其它:
  --dry-run            只检查参数并打印将要生成的配置, 不改动系统
  --uninstall          卸载 (停止服务, 删除程序、服务脚本和配置)
  -h, --help           显示本帮助

升级: 不带任何配置参数重新运行即可, 已有配置会保留。
      带了配置参数则整份配置按新参数重写 (旧配置备份为 .bak)。
EOF
}

die() { echo "错误: $*" >&2; exit 1; }

# ---------------------------------------------------------------- 参数解析
PORT=""; LISTEN=""; ALLOW=""; OPEN=0; PUBLIC_URL=""; MAX_STREAMS=""
RATE_LIMIT=""; RATE_HOURS=""; MAX_FILE_MB=""; IMG_ALLOW=""; IMG_DENY=""
REGISTRIES=""; UPSTREAM=""; TRUST=0; LOGACC=0
BIN=""; REPO="${HP_REPO:-$REPO_DEFAULT}"; TAG="${HP_VERSION:-latest}"
DRY=0; UNINSTALL=0; HAVE_CFG=0

while [ $# -gt 0 ]; do
  k="$1"; v=""; hasv=0
  case "$k" in --*=*) v="${k#*=}"; k="${k%%=*}"; hasv=1 ;; esac
  case "$k" in
    -h|--help) usage; exit 0 ;;
    --open)      OPEN=1; HAVE_CFG=1 ;;
    --trust-proxy) TRUST=1; HAVE_CFG=1 ;;
    --log-access)  LOGACC=1; HAVE_CFG=1 ;;
    --dry-run)   DRY=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --port|--listen|--allow|--public-url|--max-streams|--rate-limit|--rate-hours|--max-file-mb|\
    --image-allow|--image-deny|--registries|--upstream-proxy|--bin|--repo|--tag)
      if [ "$hasv" -eq 0 ]; then
        [ $# -ge 2 ] || die "参数 $k 需要一个值"
        v="$2"; shift
      fi
      case "$k" in
        --port) PORT="$v" ;;
        --listen) LISTEN="$v" ;;
        --allow) ALLOW="$v" ;;
        --public-url) PUBLIC_URL="$v" ;;
        --max-streams) MAX_STREAMS="$v" ;;
        --rate-limit) RATE_LIMIT="$v" ;;
        --rate-hours) RATE_HOURS="$v" ;;
        --max-file-mb) MAX_FILE_MB="$v" ;;
        --image-allow) IMG_ALLOW="$v" ;;
        --image-deny) IMG_DENY="$v" ;;
        --registries) REGISTRIES="$v" ;;
        --upstream-proxy) UPSTREAM="$v" ;;
        --bin) BIN="$v" ;;
        --repo) REPO="$v" ;;
        --tag) TAG="$v" ;;
      esac
      case "$k" in --bin|--repo|--tag) ;; *) HAVE_CFG=1 ;; esac
      ;;
    *) die "未知参数: $k  (sh install.sh --help 查看用法)" ;;
  esac
  shift
done

# ---------------------------------------------------------------- 参数校验 (改动系统之前)
# 值会被写进 shell 配置文件, 禁止可能破坏引号的字符
safe() { # 名称 值
  case "$2" in
    *[\"\$\`\\\ ]*|*'
'*) die "$1 的值里不能含有空格、引号、\$、反引号或反斜杠: $2" ;;
  esac
}
is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }

[ -z "$PORT" ] || {
  is_uint "$PORT" || die "--port 必须是数字: $PORT"
  [ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || die "--port 必须在 1024-65535 之间 (服务不以 root 运行, 绑不了 1024 以下): $PORT"
}
[ -n "$PORT" ] || PORT=5000
[ -n "$LISTEN" ] || LISTEN="0.0.0.0"
case "$LISTEN" in *[!0-9a-fA-F:.\[\]]*) die "--listen 应是 IP 地址: $LISTEN" ;; esac
for pair in "max-streams:$MAX_STREAMS" "rate-limit:$RATE_LIMIT" "max-file-mb:$MAX_FILE_MB"; do
  n="${pair%%:*}"; val="${pair#*:}"
  [ -z "$val" ] || is_uint "$val" || die "--$n 必须是非负整数: $val"
done
if [ -n "$RATE_HOURS" ]; then
  printf %s "$RATE_HOURS" | grep -Eq '^[0-9]+(\.[0-9]+)?$' || die "--rate-hours 必须是数字: $RATE_HOURS"
fi
if [ -n "$ALLOW" ]; then
  safe --allow "$ALLOW"
  printf %s "$ALLOW" | grep -Eq '^[0-9a-fA-F:.]+(/[0-9]{1,3})?(,[0-9a-fA-F:.]+(/[0-9]{1,3})?)*$' \
    || die "--allow 格式不对, 应为 IP 或 IP/掩码, 多个用逗号分隔 (不要有空格): $ALLOW"
fi
if [ -n "$PUBLIC_URL" ]; then
  safe --public-url "$PUBLIC_URL"
  printf %s "$PUBLIC_URL" | grep -Eq '^https?://[^/]+$' || die "--public-url 应形如 http://1.2.3.4:8080 (不带路径和结尾斜杠): $PUBLIC_URL"
fi
safe --image-allow "$IMG_ALLOW"; safe --image-deny "$IMG_DENY"; safe --registries "$REGISTRIES"
safe --upstream-proxy "$UPSTREAM"; safe --repo "$REPO"; safe --tag "$TAG"; safe --bin "$BIN"
if [ -n "$UPSTREAM" ]; then
  printf %s "$UPSTREAM" | grep -Eq '^(socks5h?|http|https)://[^/]+$' || die "--upstream-proxy 应形如 socks5://127.0.0.1:1080: $UPSTREAM"
fi
printf %s "$REPO" | grep -Eq '^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$' || die "--repo 应形如 用户名/仓库名: $REPO"

if [ "$HAVE_CFG" -eq 1 ] && [ -z "$ALLOW" ] && [ "$OPEN" -eq 0 ]; then
  die "必须指定 --allow 来源IP, 或明确写 --open 表示不限制。
  例: sh install.sh --port $PORT --allow 你的公网IP"
fi
[ -z "$ALLOW" ] || [ "$OPEN" -eq 0 ] || die "--allow 和 --open 不能同时使用"

# 生成配置文件内容 (健康检查要从本机访问, 所以白名单里自动补上回环地址)
gen_conf() {
  ac=""
  [ -z "$ALLOW" ] || ac="$ALLOW,127.0.0.1,::1"
  echo "# 由 install.sh 生成。要改配置: 重新运行 install.sh 带上新参数, 或手工编辑后 rc-service hubproxy-lite restart"
  echo "export HP_LISTEN=\"$LISTEN:$PORT\""
  echo "export HP_ALLOW_CIDR=\"$ac\""
  echo "export HP_MAX_STREAMS=\"${MAX_STREAMS:-6}\""
  [ -z "$PUBLIC_URL" ]  || echo "export HP_PUBLIC_URL=\"$PUBLIC_URL\""
  [ -z "$RATE_LIMIT" ]  || echo "export HP_RATE_LIMIT=\"$RATE_LIMIT\""
  [ -z "$RATE_HOURS" ]  || echo "export HP_RATE_HOURS=\"$RATE_HOURS\""
  [ -z "$MAX_FILE_MB" ] || echo "export HP_MAX_FILE_MB=\"$MAX_FILE_MB\""
  [ -z "$IMG_ALLOW" ]   || echo "export HP_ALLOW=\"$IMG_ALLOW\""
  [ -z "$IMG_DENY" ]    || echo "export HP_DENY=\"$IMG_DENY\""
  [ -z "$REGISTRIES" ]  || echo "export HP_REGISTRIES=\"$REGISTRIES\""
  [ -z "$UPSTREAM" ]    || echo "export HP_UPSTREAM_PROXY=\"$UPSTREAM\""
  [ "$TRUST" -eq 0 ]    || echo "export HP_TRUST_PROXY=\"true\""
  [ "$LOGACC" -eq 0 ]   || echo "export HP_LOG_ACCESS=\"true\""
  return 0
}

if [ "$DRY" -eq 1 ]; then
  echo "参数检查通过。将生成的 /etc/conf.d/hubproxy-lite:"
  echo "-----"
  gen_conf
  echo "-----"
  exit 0
fi

# ---------------------------------------------------------------- 系统检查
[ "$(id -u)" -eq 0 ] || die "请用 root 运行"
command -v rc-update >/dev/null 2>&1 || die "没找到 OpenRC (rc-update), 这个脚本只适用于 Alpine"

INIT=/etc/init.d/hubproxy-lite
CONF=/etc/conf.d/hubproxy-lite
EXE=/usr/local/bin/hubproxy-lite

if [ "$UNINSTALL" -eq 1 ]; then
  rc-service hubproxy-lite stop 2>/dev/null || true
  rc-update del hubproxy-lite default 2>/dev/null || true
  rm -f "$INIT" "$CONF" "$CONF.bak" "$EXE"
  deluser hubproxy 2>/dev/null || true
  echo "已卸载 (日志 /var/log/hubproxy-lite.log 保留, 可手动删除)"
  exit 0
fi

# 没有配置参数时: 已有配置就沿用, 没有就必须给参数
KEEP_CONF=0
if [ "$HAVE_CFG" -eq 0 ]; then
  [ -f "$CONF" ] || die "首次安装必须指定来源限制, 例:
  sh install.sh --port 8080 --allow 你的公网IP
  (sh install.sh --help 查看全部参数)"
  KEEP_CONF=1
fi

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的架构: $(uname -m)" ;;
esac
ASSET="hubproxy-lite-linux-$ARCH"

# ---------------------------------------------------------------- 取得二进制
apk add --no-cache ca-certificates >/dev/null

DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || DIR=.
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

if [ -z "$BIN" ]; then
  for c in "$DIR/$ASSET" "$DIR/../dist/$ASSET"; do
    [ -f "$c" ] && BIN="$c" && break
  done
fi
if [ -z "$BIN" ]; then
  if [ "$TAG" = latest ]; then BASE="https://github.com/$REPO/releases/latest/download"
  else BASE="https://github.com/$REPO/releases/download/$TAG"; fi
  echo "从 $BASE 下载..."
  wget -q -O "$TMP/$ASSET" "$BASE/$ASSET" || die "下载失败 (这台机器能访问 github.com 吗? 不能的话在别处下载后用 --bin 指定本地文件)"
  wget -q -O "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || die "下载校验文件失败"
  ( cd "$TMP" && grep " $ASSET\$" SHA256SUMS | sha256sum -c - >/dev/null ) || die "SHA256 校验不通过, 已中止"
  echo "SHA256 校验通过"
  BIN="$TMP/$ASSET"
fi
[ -f "$BIN" ] || die "找不到二进制文件: $BIN"

# ---------------------------------------------------------------- 安装
addgroup -S hubproxy 2>/dev/null || true
adduser -S -D -H -s /sbin/nologin -G hubproxy hubproxy 2>/dev/null || true

# 端口占用检查, 必须在停旧服务之前做, 失败时旧服务不受影响。
# 端口没变时占用者就是旧服务自己, 不检查。
OLDPORT=""
if [ -f "$CONF" ]; then
  OLDPORT=$( ( . "$CONF"; echo "${HP_LISTEN##*:}" ) 2>/dev/null || true )
fi
if [ "$KEEP_CONF" -eq 1 ]; then
  PORT="${OLDPORT:-5000}"
elif [ "$PORT" != "$OLDPORT" ] && netstat -tln 2>/dev/null | grep -Eq "[:.]$PORT[[:space:]]"; then
  die "端口 $PORT 已被其它程序占用, 请换一个: --port <新端口> (旧服务未受影响)"
fi

rc-service hubproxy-lite stop >/dev/null 2>&1 || true

install -m 0755 "$BIN" "$EXE"

cat > "$INIT" <<'EOF'
#!/sbin/openrc-run
# hubproxy-lite 的 OpenRC 服务脚本 (由 install.sh 生成); 配置见 /etc/conf.d/hubproxy-lite
name="hubproxy-lite"
description="Docker / GitHub acceleration proxy (lite)"
command="/usr/local/bin/hubproxy-lite"
command_user="hubproxy:hubproxy"
supervisor="supervise-daemon"
respawn_delay=3
respawn_max=0
output_log="/var/log/hubproxy-lite.log"
error_log="/var/log/hubproxy-lite.log"
rc_ulimit="-n 4096"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath -f -m 0640 -o hubproxy:hubproxy /var/log/hubproxy-lite.log
}
EOF
chmod 0755 "$INIT"

if [ "$KEEP_CONF" -eq 1 ]; then
  echo "沿用已有配置 $CONF"
else
  [ ! -f "$CONF" ] || cp "$CONF" "$CONF.bak"
  gen_conf > "$CONF"
  chmod 0644 "$CONF"
fi

rc-update add hubproxy-lite default >/dev/null
rc-service hubproxy-lite start || echo "警告: rc-service start 返回失败, 继续做健康检查"

# ---------------------------------------------------------------- 健康检查
ok=0
i=0
while [ "$i" -lt 8 ]; do
  if wget -q -O- "http://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -q ok; then ok=1; break; fi
  i=$((i+1)); sleep 1
done

echo
if [ "$ok" -eq 1 ]; then
  echo "安装完成, 服务运行中, 监听端口 $PORT"
else
  echo "服务已安装, 但健康检查没通过。排查:"
  echo "  rc-service hubproxy-lite status"
  echo "  tail -n 30 /var/log/hubproxy-lite.log"
fi
cat <<EOF

使用 (把 <IP> 换成这台服务器的地址):
  GitHub : wget http://<IP>:$PORT/https://github.com/owner/repo/archive/refs/heads/main.tar.gz
  Docker : /etc/docker/daemon.json 加
           {"registry-mirrors":["http://<IP>:$PORT"],"insecure-registries":["<IP>:$PORT"]}
别忘了在宿主机/云厂商防火墙放行 TCP $PORT。
改配置: 重新运行 install.sh 带新参数 (旧配置自动备份); 卸载: sh install.sh --uninstall
EOF
[ "$ok" -eq 1 ] || exit 1
