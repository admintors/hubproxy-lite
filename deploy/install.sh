#!/bin/sh
# hubproxy-lite 一键安装 / 升级 / 卸载 (Alpine + OpenRC)。
#
# 最常用 (root): 不带参数就进入问答向导, 端口、来源 IP、HTTPS 要不要开, 逐项询问:
#   wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | sh
#
# 想一条命令搞定 (跳过问答):
#   wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | \
#     sh -s -- --port 8080 --allow 203.0.113.7
#
# 用 sh install.sh --help 查看全部参数。
set -eu

REPO_DEFAULT="admintors/hubproxy-lite"
INIT=/etc/init.d/hubproxy-lite
CONF=/etc/conf.d/hubproxy-lite
EXE=/usr/local/bin/hubproxy-lite
LOG=/var/log/hubproxy-lite.log

usage() {
cat <<'EOF'
用法: sh install.sh [参数]
      不带任何参数 = 进入问答向导 (端口、来源 IP、HTTPS 要不要开, 逐项询问)

配置 (--allow 和 --open 二选一; 用向导时不需要写):
  --allow  IP或网段    只允许这些来源使用, 逗号分隔。例: 203.0.113.7,192.168.0.0/16
  --open               明确表示不做来源限制 (任何人都能用你的流量, 不推荐)

常用:
  --port N             监听端口, 默认 5000, 必须 >= 1024 (服务不以 root 运行)
  --listen IP          监听地址, 默认 0.0.0.0
  --public-url URL     对外地址, 例 http://203.0.113.10:8080 (不填则按请求 Host 自动推断;
                       启用 HTTPS 时自动为 https://域名[:端口])
  --max-streams N      同时下载数上限, 默认 6 (0.1 核机器建议 4~8)

HTTPS (域名 + DNS-01, 不需要开放 80/443; 不给 --domain 就是纯 HTTP):
  --domain 域名        启用 HTTPS 的域名, 例 hub.example.com (不能是 IP、不能带 *)
  --dns cloudflare|exec
                       DNS 验证方式: cloudflare 用 API Token 自动写记录;
                       exec 调用你自己的脚本 (见 README)
  --cf-token T         Cloudflare API Token (也可用环境变量 HP_CF_TOKEN 传入, 更安全,
                       不会出现在 ps 输出里)
  --cf-zone ID         Cloudflare Zone ID (可选, 不填则按域名自动查找)
  --dns-hook PATH      exec 模式的脚本绝对路径, 协议: 脚本 present|cleanup <fqdn> <value>
                       服务以 hubproxy 用户运行, 脚本及其所在目录必须对其它用户可读/可执行
  --email 邮箱         Let's Encrypt 账户邮箱 (可选, 建议填, 用于到期提醒)
  --cert-dir DIR       证书保存目录, 默认 /var/lib/hubproxy-lite
  --acme-staging       使用 Let's Encrypt 测试环境 (证书不受信任, 但不占正式限额)
  --no-https           明确关闭 HTTPS (已有 HTTPS 配置时用它改回纯 HTTP)
  --skip-dns-check     跳过安装前的 DNS 写入测试 (默认会真实写入再删除一条测试 TXT 记录)

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
  --dry-run            只检查参数并打印将要生成的配置 (token 打码), 不改动系统
  --uninstall          卸载 (停止服务, 删除程序、服务脚本和配置; 证书目录保留)
  -h, --help           显示本帮助

已安装的机器上重新运行:
  不带参数            进入菜单: 只升级 / 只修改 HTTPS / 全部重新配置
  只带 HTTPS 参数     只改 HTTPS (开启、更换、--no-https 关闭), 其它配置原样保留
  带了其它配置参数    整份配置按新参数重写 (旧配置备份为 .bak)
EOF
}

die() { echo "错误: $*" >&2; exit 1; }

# ---------------------------------------------------------------- 参数解析
PORT=""; LISTEN=""; ALLOW=""; OPEN=0; PUBLIC_URL=""; MAX_STREAMS=""
RATE_LIMIT=""; RATE_HOURS=""; MAX_FILE_MB=""; IMG_ALLOW=""; IMG_DENY=""
REGISTRIES=""; UPSTREAM=""; TRUST=0; LOGACC=0
DOMAIN=""; EMAIL=""; DNS=""; CF_TOKEN=""; CF_ZONE=""; DNS_HOOK=""; CERT_DIR=""; STAGING=0
BIN=""; REPO="${HP_REPO:-$REPO_DEFAULT}"; TAG="${HP_VERSION:-latest}"
DRY=0; UNINSTALL=0
HAVE_CFG=0      # 给了任何配置参数 (含 HTTPS 参数)
OTHER_CFG=0     # 给了 HTTPS 以外的配置参数
NO_HTTPS=0; SKIP_DNS_CHECK=0
HTTPS_ARGS=""   # 显式给出的 HTTPS 参数名 (校验用)

while [ $# -gt 0 ]; do
  k="$1"; v=""; hasv=0
  case "$k" in --*=*) v="${k#*=}"; k="${k%%=*}"; hasv=1 ;; esac
  case "$k" in
    -h|--help) usage; exit 0 ;;
    --open)      OPEN=1; HAVE_CFG=1; OTHER_CFG=1 ;;
    --trust-proxy) TRUST=1; HAVE_CFG=1; OTHER_CFG=1 ;;
    --log-access)  LOGACC=1; HAVE_CFG=1; OTHER_CFG=1 ;;
    --acme-staging) STAGING=1; HTTPS_ARGS="$HTTPS_ARGS --acme-staging"; HAVE_CFG=1 ;;
    --no-https)  NO_HTTPS=1; HAVE_CFG=1 ;;
    --skip-dns-check) SKIP_DNS_CHECK=1 ;;
    --dry-run)   DRY=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --port|--listen|--allow|--public-url|--max-streams|--rate-limit|--rate-hours|--max-file-mb|\
    --image-allow|--image-deny|--registries|--upstream-proxy|--bin|--repo|--tag|\
    --domain|--email|--dns|--cf-token|--cf-zone|--dns-hook|--cert-dir)
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
        --domain) DOMAIN="$v" ;;
        --email) EMAIL="$v" ;;
        --dns) DNS="$v" ;;
        --cf-token) CF_TOKEN="$v" ;;
        --cf-zone) CF_ZONE="$v" ;;
        --dns-hook) DNS_HOOK="$v" ;;
        --cert-dir) CERT_DIR="$v" ;;
      esac
      case "$k" in
        --bin|--repo|--tag) ;;
        --domain|--email|--dns|--cf-token|--cf-zone|--dns-hook|--cert-dir) HTTPS_ARGS="$HTTPS_ARGS $k"; HAVE_CFG=1 ;;
        *) HAVE_CFG=1; OTHER_CFG=1 ;;
      esac
      ;;
    *) die "未知参数: $k  (sh install.sh --help 查看用法)" ;;
  esac
  shift
done

# 环境变量传 token 更安全 (不进 ps); 命令行给了就用命令行的
[ -n "$CF_TOKEN" ] || CF_TOKEN="${HP_CF_TOKEN:-}"

# ---------------------------------------------------------------- 校验函数
# 值会被写进 shell 配置文件, 禁止可能破坏引号的字符
safe() { # 名称 值
  case "$2" in
    *[\"\$\`\\\ ]*|*'
'*) die "$1 的值里不能含有空格、引号、\$、反引号或反斜杠: $2" ;;
  esac
}
is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
is_yes() { case "$1" in y|Y|yes|YES|Yes) return 0 ;; esac; return 1; }
ok_port() { is_uint "$1" && [ "$1" -ge 1024 ] && [ "$1" -le 65535 ]; }
ok_ipv4() { # a.b.c.d 每段 0-255
  printf %s "$1" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$' || return 1
  _o=$IFS; IFS=.; set -- $1; IFS=$_o
  for _n in "$@"; do [ "$_n" -le 255 ] || return 1; done
}
ok_allow_item() { # IP 或 IP/掩码
  _ip="${1%%/*}"; _m=""
  case "$1" in */*) _m="${1#*/}"; is_uint "$_m" || return 1 ;; esac
  case "$_ip" in
    *:*) printf %s "$_ip" | grep -Eq '^[0-9a-fA-F:.]+$' || return 1
         [ -z "$_m" ] || [ "$_m" -le 128 ] ;;
    *)   ok_ipv4 "$_ip" || return 1
         [ -z "$_m" ] || [ "$_m" -le 32 ] ;;
  esac
}
ok_allow() { # 逗号分隔的列表
  [ -n "$1" ] || return 1
  case "$1" in ,*|*,|*,,*) return 1 ;; esac
  _o=$IFS; IFS=,; set -- $1; IFS=$_o
  for _i in "$@"; do ok_allow_item "$_i" || return 1; done
}
ok_domain() { # 小写 FQDN; 末段必须是字母, 所以 IP 天然不匹配; 不允许通配符
  printf %s "$1" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'
}
ok_email() { printf %s "$1" | grep -Eq '^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$'; }
ok_token() { printf %s "$1" | grep -Eq '^[A-Za-z0-9_-]+$'; }
ok_zone() { printf %s "$1" | grep -Eq '^[a-f0-9]+$'; }

# 服务以 hubproxy 用户运行。脚本本身要对"其它用户"可读可执行, 每一级上层目录要可进入。
# (放在 /root 下是最常见的坑: /root 是 0700, 服务会报 Permission denied)
hook_reachable() { # 路径; 不可达时把原因写到 stderr 并返回 1
  _p="$1"
  _m=$(stat -L -c %a "$_p" 2>/dev/null) || return 0
  _x=${_m#"${_m%?}"}
  if [ $(( _x & 5 )) -ne 5 ]; then
    echo "脚本 $_p 对其它用户缺少读/执行权限 (当前 $_m); 服务以 hubproxy 用户运行, 会 Permission denied。执行: chmod 755 $_p" >&2
    return 1
  fi
  _d=$(dirname "$_p")
  while [ "$_d" != "/" ] && [ "$_d" != "." ]; do
    _m=$(stat -L -c %a "$_d" 2>/dev/null) || break
    _x=${_m#"${_m%?}"}
    if [ $(( _x & 1 )) -ne 1 ]; then
      echo "脚本所在目录 $_d 对其它用户不可进入 (当前 $_m); 服务以 hubproxy 用户运行, 会 Permission denied。请把脚本放到 /usr/local/bin, 或执行: chmod o+x $_d" >&2
      return 1
    fi
    _d=$(dirname "$_d")
  done
  return 0
}

# ---------------------------------------------------------------- 参数校验 (改动系统之前)
[ -z "$PORT" ] || {
  is_uint "$PORT" || die "--port 必须是数字: $PORT"
  [ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || die "--port 必须在 1024-65535 之间 (服务不以 root 运行, 绑不了 1024 以下): $PORT"
}
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
  ok_allow "$ALLOW" \
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

# ---- HTTPS 参数校验
if [ "$NO_HTTPS" -eq 1 ]; then
  [ -z "$HTTPS_ARGS" ] || die "--no-https 不能和这些 HTTPS 参数同时使用:$HTTPS_ARGS"
fi
if [ -n "$DOMAIN" ]; then
  DOMAIN=$(printf %s "$DOMAIN" | tr 'A-Z' 'a-z' | sed 's/\.$//')
  ok_domain "$DOMAIN" || die "--domain 必须是域名 (不能是 IP, 不能含 *), 例 hub.example.com: $DOMAIN"
  [ -n "$DNS" ] || die "启用 HTTPS 必须指定 --dns cloudflare 或 --dns exec"
  case "$DNS" in
    cloudflare)
      [ -z "$DNS_HOOK" ] || die "--dns cloudflare 不能同时使用 --dns-hook"
      [ -n "$CF_TOKEN" ] || die "--dns cloudflare 需要 Token: 用 --cf-token 或环境变量 HP_CF_TOKEN"
      ok_token "$CF_TOKEN" || die "Cloudflare Token 含有非法字符 (只允许字母数字 _ -)"
      ;;
    exec)
      [ -n "$DNS_HOOK" ] || die "--dns exec 需要 --dns-hook 指定脚本路径"
      [ -z "$CF_TOKEN" ] && [ -z "$CF_ZONE" ] || die "--dns exec 不能使用 --cf-token / --cf-zone"
      case "$DNS_HOOK" in /*) ;; *) die "--dns-hook 必须是绝对路径: $DNS_HOOK" ;; esac
      safe --dns-hook "$DNS_HOOK"
      [ -f "$DNS_HOOK" ] && [ -x "$DNS_HOOK" ] || die "--dns-hook 指向的文件不存在或不可执行: $DNS_HOOK"
      hook_reachable "$DNS_HOOK" || die "--dns-hook 脚本服务用户 (hubproxy) 访问不到, 见上面的说明"
      ;;
    *) die "--dns 只能是 cloudflare 或 exec: $DNS" ;;
  esac
  if [ -n "$EMAIL" ]; then
    ok_email "$EMAIL" || die "--email 格式不对: $EMAIL"
  fi
  if [ -n "$CF_ZONE" ]; then
    [ "$DNS" = cloudflare ] || die "--cf-zone 只能配合 --dns cloudflare"
    ok_zone "$CF_ZONE" || die "--cf-zone 应为十六进制 Zone ID"
  fi
  if [ -n "$CERT_DIR" ]; then
    case "$CERT_DIR" in /*) ;; *) die "--cert-dir 必须是绝对路径: $CERT_DIR" ;; esac
    safe --cert-dir "$CERT_DIR"
  fi
else
  [ -z "$HTTPS_ARGS" ] || die "以下 HTTPS 参数需要同时指定 --domain:$HTTPS_ARGS"
fi
[ -n "$CERT_DIR" ] || CERT_DIR="/var/lib/hubproxy-lite"

# ---------------------------------------------------------------- 模式
# full  = 整份配置重写 (全新安装 / 全部重新配置 / 带了其它配置参数)
# keep  = 保持现有配置, 只升级程序
# https = 只改 HTTPS 相关几行, 其它配置原样保留
EXISTING=0; [ -f "$CONF" ] && EXISTING=1
MODE=full
if [ "$HAVE_CFG" -eq 1 ] && [ "$OTHER_CFG" -eq 0 ] && [ "$EXISTING" -eq 1 ]; then MODE=https; fi

if [ "$MODE" = full ] && [ "$HAVE_CFG" -eq 1 ] && [ -z "$ALLOW" ] && [ "$OPEN" -eq 0 ]; then
  die "必须指定 --allow 来源IP, 或明确写 --open 表示不限制。
  例: sh install.sh --port ${PORT:-5000} --allow 你的公网IP"
fi
[ -z "$ALLOW" ] || [ "$OPEN" -eq 0 ] || die "--allow 和 --open 不能同时使用"

# ---------------------------------------------------------------- 生成配置
# HTTPS 配置块。$1 = 1 时 token 打码 (dry-run 显示用)
gen_https_block() {
  echo "export HP_DOMAIN=\"$DOMAIN\""
  [ -z "$EMAIL" ] || echo "export HP_EMAIL=\"$EMAIL\""
  echo "export HP_DNS_PROVIDER=\"$DNS\""
  if [ "$DNS" = cloudflare ]; then
    if [ "${1:-0}" -eq 1 ]; then echo "export HP_CF_TOKEN=\"****\""; else echo "export HP_CF_TOKEN=\"$CF_TOKEN\""; fi
    [ -z "$CF_ZONE" ] || echo "export HP_CF_ZONE=\"$CF_ZONE\""
  else
    echo "export HP_DNS_HOOK=\"$DNS_HOOK\""
  fi
  echo "export HP_CERT_DIR=\"$CERT_DIR\""
  [ "$STAGING" -eq 0 ] || echo "export HP_ACME_STAGING=\"true\""
  return 0
}

# 整份配置 (健康检查要从本机访问, 所以白名单里自动补上回环地址)
gen_conf() {
  ac=""
  [ -z "$ALLOW" ] || ac="$ALLOW,127.0.0.1,::1"
  echo "# 由 install.sh 生成。要改配置: 重新运行 install.sh, 或手工编辑后 rc-service hubproxy-lite restart"
  echo "export HP_LISTEN=\"$LISTEN:${PORT:-5000}\""
  echo "export HP_ALLOW_CIDR=\"$ac\""
  echo "export HP_MAX_STREAMS=\"${MAX_STREAMS:-6}\""
  [ -z "$DOMAIN" ] || gen_https_block "${1:-0}"
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
  if [ "$MODE" = https ]; then
    if [ -n "$DOMAIN" ]; then
      echo "参数检查通过。已有配置 $CONF 只会更新 HTTPS 相关几行, 其它保持不变。将写入:"
      echo "-----"; gen_https_block 1; echo "-----"
    else
      echo "参数检查通过。将从 $CONF 中移除 HTTPS 相关配置 (改回纯 HTTP), 其它保持不变。"
    fi
  else
    echo "参数检查通过。将生成的 $CONF:"
    echo "-----"; gen_conf 1; echo "-----"
  fi
  exit 0
fi

# ---------------------------------------------------------------- 系统检查
[ "$(id -u)" -eq 0 ] || die "请用 root 运行"
command -v rc-update >/dev/null 2>&1 || die "没找到 OpenRC (rc-update), 这个脚本只适用于 Alpine"

if [ "$UNINSTALL" -eq 1 ]; then
  rc-service hubproxy-lite stop 2>/dev/null || true
  rc-update del hubproxy-lite default 2>/dev/null || true
  rm -f "$INIT" "$CONF" "$CONF.bak" "$EXE"
  deluser hubproxy 2>/dev/null || true
  echo "已卸载 (日志 $LOG 保留, 证书目录不删除, 可手动删除)"
  exit 0
fi

# ---------------------------------------------------------------- 终端与输入
# 通过 wget | sh 运行时 stdin 是管道, 所以问答一律从 /dev/tty 读
TTY=0
if [ -r /dev/tty ] && ( : </dev/tty ) 2>/dev/null; then TTY=1; exec 3</dev/tty; fi

ask() { # 提示语 默认值 -> $REPLY
  printf '%s' "$1" >&2
  IFS= read -r REPLY <&3 || { echo >&2; die "输入被中断"; }
  [ -n "$REPLY" ] || REPLY="$2"
}
ask_secret() { # 提示语 -> $REPLY (关闭回显)
  printf '%s' "$1" >&2
  stty -echo <&3 2>/dev/null || true
  IFS= read -r REPLY <&3 || { stty echo <&3 2>/dev/null || true; echo >&2; die "输入被中断"; }
  stty echo <&3 2>/dev/null || true
  echo >&2
}
clean() { printf %s "$1" | sed -e 's/，/,/g' -e 's/[[:space:]]//g'; }

# 没参数、没旧配置、也没有终端: 没法问, 提前说清楚 (在下载之前)
if [ "$HAVE_CFG" -eq 0 ] && [ "$EXISTING" -eq 0 ] && [ "$TTY" -eq 0 ]; then
  die "没有可用的终端来提问。请带参数运行, 例:
  sh install.sh --port 8080 --allow 你的公网IP
  sh install.sh --port 8443 --allow 你的公网IP --domain hub.example.com --dns cloudflare --cf-token <Token>
  (sh install.sh --help 查看全部参数)"
fi

# ---------------------------------------------------------------- 取得二进制 (在提问之前, 这样 DNS 预检能直接用它)
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的架构: $(uname -m)" ;;
esac
ASSET="hubproxy-lite-linux-$ARCH"

apk add --no-cache ca-certificates >/dev/null || die "安装 ca-certificates 失败 (apk 能联网吗?)"

DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || DIR=.
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

SRC="本地文件"
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
  SRC="GitHub $REPO ($TAG), 已校验 SHA256"
fi
[ -f "$BIN" ] || die "找不到二进制文件: $BIN"
chmod 0755 "$BIN"

# ---------------------------------------------------------------- 旧配置读取
OLD_DOMAIN=""; OLD_DNS=""; OLD_EMAIL=""; OLD_CF_TOKEN=""; OLD_CF_ZONE=""; OLD_HOOK=""
OLD_CERT=""; OLD_STAGING=0; OLD_PORT=""; OLD_ALLOW=""; OLD_OPEN=0

oldv() { # 变量名 -> 在子 shell 里 source 旧配置后取值
  ( . "$CONF" >/dev/null 2>&1; eval "printf %s \"\${$1:-}\"" ) 2>/dev/null || true
}
read_old_summary() {
  v=$(oldv HP_LISTEN); OLD_PORT="${v##*:}"
  a=$(oldv HP_ALLOW_CIDR); a="${a%,127.0.0.1,::1}"
  if [ -n "$a" ]; then OLD_ALLOW="$a"; OLD_OPEN=0; else OLD_ALLOW=""; OLD_OPEN=1; fi
  OLD_DOMAIN=$(oldv HP_DOMAIN); OLD_DNS=$(oldv HP_DNS_PROVIDER); OLD_EMAIL=$(oldv HP_EMAIL)
  OLD_CF_TOKEN=$(oldv HP_CF_TOKEN); OLD_CF_ZONE=$(oldv HP_CF_ZONE); OLD_HOOK=$(oldv HP_DNS_HOOK)
  OLD_CERT=$(oldv HP_CERT_DIR)
  case "$(oldv HP_ACME_STAGING)" in true|1|yes) OLD_STAGING=1 ;; *) OLD_STAGING=0 ;; esac
}
# 全部重新配置时, 把旧配置里其它设置读出来带到新配置里, 不让用户已有的限流/黑白名单等丢失
load_old_settings() {
  v=$(oldv HP_LISTEN); [ -z "$v" ] || { PORT="${v##*:}"; LISTEN="${v%:*}"; }
  ALLOW="$OLD_ALLOW"; OPEN="$OLD_OPEN"
  MAX_STREAMS=$(oldv HP_MAX_STREAMS); RATE_LIMIT=$(oldv HP_RATE_LIMIT); RATE_HOURS=$(oldv HP_RATE_HOURS)
  MAX_FILE_MB=$(oldv HP_MAX_FILE_MB); IMG_ALLOW=$(oldv HP_ALLOW); IMG_DENY=$(oldv HP_DENY)
  REGISTRIES=$(oldv HP_REGISTRIES); UPSTREAM=$(oldv HP_UPSTREAM_PROXY); PUBLIC_URL=$(oldv HP_PUBLIC_URL)
  case "$(oldv HP_TRUST_PROXY)" in true|1|yes) TRUST=1 ;; esac
  case "$(oldv HP_LOG_ACCESS)" in true|1|yes) LOGACC=1 ;; esac
  # 手工改过的值可能含特殊字符; 重写前再检查一遍, 避免生成出坏文件
  for _pair in "HP_LISTEN:$LISTEN" "HP_ALLOW_CIDR:$ALLOW" "HP_ALLOW:$IMG_ALLOW" "HP_DENY:$IMG_DENY" \
               "HP_REGISTRIES:$REGISTRIES" "HP_UPSTREAM_PROXY:$UPSTREAM" "HP_PUBLIC_URL:$PUBLIC_URL"; do
    safe "旧配置里的 ${_pair%%:*}" "${_pair#*:}"
  done
}

# ---------------------------------------------------------------- HTTPS 相关交互与预检
PRECHECKED=0

show_resolve() { # 域名: 只提示, 不拦截 (DNS-01 签发不依赖 A 记录)
  command -v getent >/dev/null 2>&1 || return 0
  _ips=$(getent hosts "$1" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')
  _srv=""
  [ -z "${SSH_CONNECTION:-}" ] || _srv=$(printf %s "$SSH_CONNECTION" | awk '{print $3}')
  if [ -z "$_ips" ]; then
    echo "  提示: 现在解析不到 $1。签发证书不依赖它 (用的是 DNS 验证), 但客户端要连上, 需要把 A 记录指向这台服务器的公网 IP。"
  else
    echo "  $1 当前解析到: $_ips"
    case " $_ips" in
      *" $_srv "*) [ -z "$_srv" ] || echo "  (包含你当前 SSH 连接的服务器地址 $_srv, 看起来没问题)" ;;
      *) echo "  (请确认这是这台服务器的公网 IP; 不是的话去改 A 记录)" ;;
    esac
  fi
}

# 真实写入再删除一条测试 TXT 记录。比"只读查询"严格: Token 缺 DNS:Edit 权限只有这样才测得出来。
precheck_dns() {
  [ "$SKIP_DNS_CHECK" -eq 0 ] || return 0
  echo "  正在验证 DNS 配置 (会写入并删除一条测试记录 _hubproxy-check.$DOMAIN) ..."
  if _out=$( ( export HP_DOMAIN="$DOMAIN" HP_DNS_PROVIDER="$DNS" HP_CF_TOKEN="$CF_TOKEN" \
                      HP_CF_ZONE="$CF_ZONE" HP_DNS_HOOK="$DNS_HOOK" HP_CERT_DIR="$CERT_DIR"
               "$BIN" check-dns ) 2>&1 ); then
    echo "  DNS 验证通过。"
    return 0
  fi
  printf '%s\n' "$_out" | sed 's/^/    /' >&2
  return 1
}

collect_https() {
  while :; do
    ask "  域名 (例 hub.example.com)${OLD_DOMAIN:+ [$OLD_DOMAIN]}: " "$OLD_DOMAIN"
    d=$(clean "$REPLY" | tr 'A-Z' 'a-z' | sed 's/\.$//')
    if ok_domain "$d"; then DOMAIN="$d"; break; fi
    echo "  不是有效的域名 (不能是 IP, 不能含 *, 要有后缀如 .com)" >&2
  done
  show_resolve "$DOMAIN"
  echo "  注意: 域名在 Cloudflare 的话, 这条 A 记录请设为 [仅 DNS] (灰云), 不要开代理 (橙云);"
  echo "        开了代理, 非标准端口和大文件下载会出问题。"
  echo
  EMAIL=""
  ask "  邮箱 (可留空; Let's Encrypt 用它发证书到期提醒)${OLD_EMAIL:+ [$OLD_EMAIL]}: " "$OLD_EMAIL"; e=$(clean "$REPLY")
  if [ -n "$e" ]; then
    if ok_email "$e"; then EMAIL="$e"; else echo "  邮箱格式不对, 已忽略" >&2; fi
  fi
  echo
  echo "  DNS 验证方式: 程序要在你的域名下临时加一条 TXT 记录来证明域名归你。"
  echo "    [1] Cloudflare  (填 API Token, 自动完成)"
  echo "    [2] 自定义脚本  (其它 DNS 服务商; 脚本负责增删 TXT 记录, 协议见 README)"
  defdns=1; [ "$OLD_DNS" != exec ] || defdns=2
  ask "  请选择 [$defdns]: " "$defdns"
  CF_TOKEN=""; CF_ZONE=""; DNS_HOOK=""
  case "$REPLY" in
    2)
      DNS=exec
      echo "  脚本以 hubproxy 用户身份运行, 脚本及其所在目录必须对其它用户可读/可执行 (不要放在 /root 下)。"
      while :; do
        ask "  脚本绝对路径${OLD_HOOK:+ [$OLD_HOOK]}: " "$OLD_HOOK"; h=$(clean "$REPLY")
        case "$h" in /*) ;; *) echo "  必须是绝对路径" >&2; continue ;; esac
        if [ ! -f "$h" ] || [ ! -x "$h" ]; then echo "  文件不存在或不可执行" >&2; continue; fi
        hook_reachable "$h" || continue
        DNS_HOOK="$h"; break
      done
      ;;
    *)
      DNS=cloudflare
      echo "  Token 权限只需要两项: Zone - Zone - Read 和 Zone - DNS - Edit, 范围选这个域名所在的 Zone。"
      echo "  创建位置: Cloudflare 控制台 -> 我的个人资料 -> API 令牌 -> 创建令牌。"
      if [ "$OLD_DNS" = cloudflare ] && [ -n "$OLD_CF_TOKEN" ] && ok_token "$OLD_CF_TOKEN"; then
        ask "  沿用已保存的 Token? [Y/n]: " y
        if ! case "$REPLY" in n|N|no|NO) true ;; *) false ;; esac; then
          CF_TOKEN="$OLD_CF_TOKEN"; CF_ZONE="$OLD_CF_ZONE"
        fi
      fi
      while [ -z "$CF_TOKEN" ]; do
        ask_secret "  Cloudflare API Token (输入不回显): "; t=$(clean "$REPLY")
        if [ -n "$t" ] && ok_token "$t"; then CF_TOKEN="$t"; else echo "  Token 为空或含非法字符" >&2; fi
      done
      ;;
  esac
  echo
  echo "  证书环境: 默认直接申请正式证书 (安装前已经验证过 DNS 能写入, 失败风险很低)。"
  echo "  如果想先反复试验, 可选测试环境: 证书不受信任 (Docker 和浏览器会报错), 但不占正式限额。"
  STAGING=0
  ask "  使用测试环境? [y/N]: " n
  is_yes "$REPLY" && STAGING=1
  return 0
}

disable_https() { DOMAIN=""; DNS=""; CF_TOKEN=""; CF_ZONE=""; DNS_HOOK=""; EMAIL=""; STAGING=0; }

ask_https() {
  echo
  echo "HTTPS"
  echo "  不启用: 地址是 http://IP:端口。Docker 客户端要把它登记为 insecure-registries, 流量不加密。"
  echo "  启用:   地址是 https://域名:端口。证书由 Let's Encrypt 签发并自动续期, Docker 无需额外设置。"
  echo "          需要: (1) 一个域名   (2) 该域名的 DNS 在 Cloudflare 托管, 或你有脚本能增删 TXT 记录"
  echo "          不需要开放 80/443 端口 (用的是 DNS 验证, 不是 HTTP 验证)。"
  if [ -n "$OLD_DOMAIN" ]; then
    echo "          当前已启用: $OLD_DOMAIN"
    ask "  继续使用 HTTPS? [Y/n]: " y
    if case "$REPLY" in n|N|no|NO) true ;; *) false ;; esac; then disable_https; return 0; fi
  else
    ask "  启用 HTTPS? [y/N]: " n
    is_yes "$REPLY" || { disable_https; return 0; }
  fi
  while :; do
    collect_https
    [ "$SKIP_DNS_CHECK" -eq 0 ] || break
    echo
    if precheck_dns; then PRECHECKED=1; break; fi
    echo
    echo "  DNS 验证没通过 (原因见上)。怎么办?"
    echo "    [1] 重新填写 HTTPS 设置"
    echo "    [2] 忽略, 仍按这些设置安装 (装完证书大概率申请失败, 要自己修配置)"
    echo "    [3] 不启用 HTTPS"
    ask "  请选择 [1]: " 1
    case "$REPLY" in
      2) PRECHECKED=1; break ;;   # 用户明知失败仍要装: 后面不再重复预检
      3) disable_https; break ;;
    esac
  done
  return 0
}

ask_port() {
  while :; do
    ask "  监听端口 [${PORT:-5000}]: " "${PORT:-5000}"; p=$(clean "$REPLY")
    if ok_port "$p"; then PORT="$p"; break; fi
    echo "  端口必须是 1024-65535 的数字 (服务不以 root 运行, 绑不了 1024 以下)" >&2
  done
}

ask_allow() {
  SSHIP=""
  [ -z "${SSH_CONNECTION:-}" ] || SSHIP="${SSH_CONNECTION%% *}"
  echo "  允许使用的来源 IP: 填会用这个代理的机器的公网 IP, 多个用逗号分隔, 可写网段如 192.168.0.0/16。"
  echo "  输入 open 表示不限制 (不推荐: 任何人都能消耗你的流量)。"
  [ "$OPEN" -eq 0 ] || echo "  当前配置: 不限制来源"
  [ -z "$SSHIP" ] || echo "  检测到你当前 SSH 登录的来源是 $SSHIP; 如果以后用代理的不是这台机器, 请改成它的 IP。"
  _def="${ALLOW:-$SSHIP}"
  while :; do
    if [ -n "$_def" ]; then ask "  来源 IP [$_def]: " "$_def"; else ask "  来源 IP: " ""; fi
    a=$(clean "$REPLY")
    case "$a" in
      "") echo "  必须填写。想不限制请输入 open" >&2; continue ;;
      open|OPEN|Open)
        ask "  确认不限制来源? 任何人都能消耗你的流量 [y/N]: " n
        if is_yes "$REPLY"; then OPEN=1; ALLOW=""; break; fi
        continue ;;
    esac
    if ok_allow "$a"; then ALLOW="$a"; OPEN=0; break; fi
    echo "  格式不对: 应为 IP 或 IP/掩码, 多个用英文逗号分隔, 例如 203.0.113.7,192.168.0.0/16" >&2
  done
}

# 完整列出这次会做什么, 让用户在动手前看清楚
print_summary() {
  echo
  echo "================ 即将执行 ================"
  case "$MODE" in
    keep)  echo "  模式      : 保持现有配置, 只升级程序" ;;
    https) echo "  模式      : 只修改 HTTPS (端口、来源限制等其它配置原样保留)" ;;
    *)     echo "  模式      : 写入全部配置" ;;
  esac
  echo "  程序      : $SRC"
  echo "  端口      : $PORT"
  if [ "$MODE" = full ]; then
    echo "  来源限制  : ${ALLOW:-不限制} (本机回环自动放行, 用于健康检查)"
  fi
  if [ -n "$DOMAIN" ]; then
    echo "  HTTPS     : 启用, 域名 $DOMAIN"
    echo "              DNS 验证: $([ "$DNS" = cloudflare ] && echo "Cloudflare API" || echo "自定义脚本 $DNS_HOOK")"
    echo "              邮箱: ${EMAIL:-未填}"
    echo "              证书环境: $([ "$STAGING" -eq 1 ] && echo '测试 (证书不受信任, 仅用于试验)' || echo '正式 (Let'"'"'s Encrypt)')"
    echo "              证书目录: $CERT_DIR (权限 0700)"
  else
    echo "  HTTPS     : 不启用 (http, Docker 需要登记 insecure-registries)"
  fi
  if [ "$MODE" != keep ]; then
    echo "  配置文件  : $CONF (权限 0600, 旧配置备份为 .bak)"
  fi
  if [ -n "$DOMAIN" ]; then
    echo "  安装之后  : 启动服务, 并等待证书签发 (通常 1~3 分钟, 最多等 4 分钟), 结果会如实报告"
  fi
  echo "=========================================="
}

# ---------------------------------------------------------------- 选择模式 / 向导
trap 'stty echo <&3 2>/dev/null || true; echo >&2; exit 130' INT TERM

if [ "$HAVE_CFG" -eq 0 ] && [ "$EXISTING" -eq 1 ]; then
  read_old_summary
  MODE=keep
  PORT="$OLD_PORT"
  if [ "$TTY" -eq 1 ]; then
    echo
    echo "检测到已有配置:"
    echo "  端口 $OLD_PORT | 来源 ${OLD_ALLOW:-不限制} | HTTPS $([ -n "$OLD_DOMAIN" ] && echo "已启用 ($OLD_DOMAIN)" || echo "未启用")"
    [ -n "$OLD_DOMAIN" ] || echo "  (HTTPS 当前没开; 想启用请选 2)"
    echo
    echo "  [1] 保持现有配置, 只升级程序 (默认)"
    echo "  [2] 只修改 HTTPS (开启 / 关闭 / 更换域名或 DNS 设置), 其它配置不动"
    echo "  [3] 全部重新配置 (端口、来源 IP、HTTPS 全部重新问一遍; 其它高级设置会带过去)"
    ask "请选择 [1]: " 1
    case "$REPLY" in
      2) MODE=https
         echo; echo "=== 修改 HTTPS 设置 (直接回车 = 用方括号里的默认值; Ctrl+C 随时退出, 退出前不会改动系统) ==="
         ask_https
         print_summary
         ask "确认? [Y/n]: " y
         case "$REPLY" in n|N|no|NO) echo "已取消"; exit 1 ;; esac ;;
      3) MODE=full
         load_old_settings
         echo; echo "=== 重新配置 (直接回车 = 用方括号里的默认值; Ctrl+C 随时退出, 退出前不会改动系统) ==="
         echo; echo "1/3  监听端口"; ask_port
         echo; echo "2/3  来源限制"; ask_allow
         echo; echo "3/3  HTTPS"; ask_https
         print_summary
         ask "确认? [Y/n]: " y
         case "$REPLY" in n|N|no|NO) echo "已取消"; exit 1 ;; esac ;;
      *) MODE=keep; DOMAIN="$OLD_DOMAIN"; STAGING="$OLD_STAGING"
         [ -z "$OLD_CERT" ] || CERT_DIR="$OLD_CERT" ;;
    esac
  else
    # 没有终端: 沿用已有配置
    DOMAIN="$OLD_DOMAIN"; STAGING="$OLD_STAGING"; [ -z "$OLD_CERT" ] || CERT_DIR="$OLD_CERT"
  fi
elif [ "$HAVE_CFG" -eq 0 ]; then
  # 全新安装, 向导
  MODE=full
  echo
  echo "=== hubproxy-lite 安装向导 ==="
  echo "将依次询问三件事: 1) 监听端口  2) 允许使用的来源 IP  3) 要不要启用 HTTPS"
  echo "直接回车 = 用方括号里的默认值。Ctrl+C 随时退出; 最后确认之前不会改动系统。"
  echo
  echo "1/3  监听端口"; ask_port
  echo; echo "2/3  来源限制"; ask_allow
  echo; echo "3/3  HTTPS"; ask_https
  print_summary
  ask "确认? [Y/n]: " y
  case "$REPLY" in n|N|no|NO) echo "已取消"; exit 1 ;; esac
elif [ "$MODE" = https ]; then
  # 命令行: 只带了 HTTPS 参数, 已有配置 -> 只改 HTTPS
  read_old_summary
  PORT="$OLD_PORT"
fi
trap - INT TERM

# keep / https 模式下端口取旧配置
[ -n "$PORT" ] || PORT=5000

# ---------------------------------------------------------------- 非交互路径的 DNS 预检
if [ -n "$DOMAIN" ] && [ "$MODE" != keep ] && [ "$PRECHECKED" -eq 0 ] && [ "$SKIP_DNS_CHECK" -eq 0 ]; then
  echo "安装前验证 DNS 配置 ..."
  precheck_dns || die "DNS 验证失败, 未做任何改动。修正参数后重试; 确认无误想跳过检查可加 --skip-dns-check"
fi

# ---------------------------------------------------------------- 安装
addgroup -S hubproxy 2>/dev/null || true
adduser -S -D -H -s /sbin/nologin -G hubproxy hubproxy 2>/dev/null || true

# 端口占用检查, 必须在停旧服务之前做, 失败时旧服务不受影响。
# 端口没变时占用者就是旧服务自己, 不检查。
OLDPORT=""
if [ -f "$CONF" ]; then OLDPORT=$(oldv HP_LISTEN); OLDPORT="${OLDPORT##*:}"; fi
if [ "$MODE" = full ] && [ "$PORT" != "$OLDPORT" ] && netstat -tln 2>/dev/null | grep -Eq "[:.]$PORT[[:space:]]"; then
  die "端口 $PORT 已被其它程序占用, 请换一个 (旧服务未受影响)"
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

case "$MODE" in
  keep)
    echo "沿用已有配置 $CONF"
    ;;
  full)
    [ ! -f "$CONF" ] || { cp "$CONF" "$CONF.bak"; chmod 0600 "$CONF.bak"; }
    umask 077
    gen_conf 0 > "$CONF"
    chmod 0600 "$CONF"
    umask 022
    ;;
  https)
    # 只替换 HTTPS 相关几行, 其它 (含手工改过的) 原样保留
    umask 077
    _t=$(mktemp)
    grep -Ev '^[[:space:]]*(export[[:space:]]+)?HP_(DOMAIN|EMAIL|DNS_PROVIDER|CF_TOKEN|CF_ZONE|DNS_HOOK|CERT_DIR|ACME_STAGING)=' "$CONF" > "$_t" || true
    if [ -n "$DOMAIN" ] && grep -Eq '^[[:space:]]*(export[[:space:]]+)?HP_PUBLIC_URL="?http://' "$_t"; then
      grep -Ev '^[[:space:]]*(export[[:space:]]+)?HP_PUBLIC_URL=' "$_t" > "$_t.2" || true
      mv "$_t.2" "$_t"
      echo "提示: 已移除旧配置里的 HP_PUBLIC_URL (它是 http 地址, 和 HTTPS 矛盾), 现在会按域名自动生成。"
    fi
    if [ -z "$DOMAIN" ] && grep -Eq '^[[:space:]]*(export[[:space:]]+)?HP_PUBLIC_URL="?https://' "$_t"; then
      echo "提示: 配置里的 HP_PUBLIC_URL 还是 https 地址, 改回 http 后请同步修改 $CONF"
    fi
    if [ -n "$DOMAIN" ]; then
      { echo; echo "# HTTPS (由 install.sh 写入)"; gen_https_block 0; } >> "$_t"
    fi
    cp "$CONF" "$CONF.bak"; chmod 0600 "$CONF.bak"
    cat "$_t" > "$CONF"; chmod 0600 "$CONF"
    rm -f "$_t"
    umask 022
    ;;
esac

# 证书目录: 归 hubproxy 用户所有, 仅自己可读写 (里面有证书私钥和 ACME 账户密钥)
if [ -n "$DOMAIN" ]; then
  install -d -m 0700 -o hubproxy -g hubproxy "$CERT_DIR" 2>/dev/null || mkdir -p "$CERT_DIR"
  chown hubproxy:hubproxy "$CERT_DIR" 2>/dev/null || true
  chmod 0700 "$CERT_DIR"
fi

rc-update add hubproxy-lite default >/dev/null
LOGSZ=$(wc -c < "$LOG" 2>/dev/null || echo 0)
rc-service hubproxy-lite start || echo "警告: rc-service start 返回失败, 继续检查"

# ---------------------------------------------------------------- 等待就绪并如实报告
RESULT=ok   # ok | cert_fail | cert_wait | down
CERT_LINE=""
service_up() { rc-service hubproxy-lite status 2>/dev/null | grep -q started; }

if [ -z "$DOMAIN" ]; then
  RESULT=down
  i=0
  while [ "$i" -lt 8 ]; do
    if wget -q -O- "http://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -qx ok; then RESULT=ok; break; fi
    i=$((i+1)); sleep 1
  done
else
  # HTTPS: 证书要走 DNS 验证, 需要等。以日志为准判断成功/失败, 不靠握手探测 (busybox wget 对 https 支持有限)
  echo
  echo "等待 HTTPS 证书签发 (DNS 验证通常 1~3 分钟, 最多等 4 分钟) ..."
  RESULT=cert_wait
  i=0
  while [ "$i" -lt 240 ]; do
    NEWLOG=$(tail -c +$((LOGSZ + 1)) "$LOG" 2>/dev/null || true)
    CERT_LINE=$(printf '%s\n' "$NEWLOG" | grep -E '证书已更新|已加载证书' | tail -1 || true)
    if [ -n "$CERT_LINE" ]; then RESULT=ok; break; fi
    FAIL_LINE=$(printf '%s\n' "$NEWLOG" | grep '证书申请失败' | tail -1 || true)
    if [ -n "$FAIL_LINE" ]; then RESULT=cert_fail; CERT_LINE="$FAIL_LINE"; break; fi
    if ! service_up; then RESULT=down; break; fi
    sleep 2; i=$((i+2))
    [ $((i % 20)) -ne 0 ] || echo "  ... 已等待 ${i} 秒"
  done
fi

echo
case "$RESULT" in
  ok)
    if [ -n "$DOMAIN" ]; then
      echo "安装完成, HTTPS 证书已就绪。"
      echo "  日志: $CERT_LINE"
    else
      echo "安装完成, 服务运行中, 监听端口 $PORT"
    fi ;;
  cert_fail)
    echo "服务已启动, 但 HTTPS 证书申请失败:"
    echo "  ${CERT_LINE#*证书申请失败: }"
    echo "服务不会退出, 会自动按 5 分钟、10 分钟 ... 退避重试。修正配置后可立即重试:"
    echo "  vi $CONF && rc-service hubproxy-lite restart"
    echo "完整日志: tail -n 30 $LOG" ;;
  cert_wait)
    echo "服务已启动, 但 4 分钟内还没拿到证书 (可能是 DNS 生效慢)。服务会在后台继续重试。"
    echo "  查看进度: tail -f $LOG" ;;
  *)
    echo "服务没有正常运行。排查:"
    echo "  rc-service hubproxy-lite status"
    echo "  tail -n 30 $LOG" ;;
esac

if [ -n "$DOMAIN" ]; then
  cat <<EOF

使用 (域名 $DOMAIN, 证书由 Let's Encrypt 签发, 客户端无需额外信任设置):
  GitHub : wget https://$DOMAIN:$PORT/https://github.com/owner/repo/archive/refs/heads/main.tar.gz
  Docker : /etc/docker/daemon.json 加
           {"registry-mirrors":["https://$DOMAIN:$PORT"]}
  还需要: 域名 A 记录指向这台服务器 (Cloudflare 里设为"仅 DNS"); 防火墙放行 TCP $PORT。
EOF
  [ "$STAGING" -eq 0 ] || echo "  当前是测试环境证书 (不受信任); 要换正式证书: 重新运行 install.sh, 选 [2] 只修改 HTTPS, 测试环境选 N。"
else
  cat <<EOF

使用 (把 <IP> 换成这台服务器的地址):
  GitHub : wget http://<IP>:$PORT/https://github.com/owner/repo/archive/refs/heads/main.tar.gz
  Docker : /etc/docker/daemon.json 加
           {"registry-mirrors":["http://<IP>:$PORT"],"insecure-registries":["<IP>:$PORT"]}
  别忘了在宿主机/云厂商防火墙放行 TCP $PORT。
  想启用 HTTPS (免去 insecure-registries): 重新运行 install.sh, 选 [2] 只修改 HTTPS。
EOF
fi
cat <<EOF
改配置: 重新运行 install.sh (旧配置自动备份为 .bak); 卸载: sh install.sh --uninstall
EOF
[ "$RESULT" = ok ] || exit 1
