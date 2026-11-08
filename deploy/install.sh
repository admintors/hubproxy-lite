#!/bin/sh
# hubproxy-lite 一键安装 / 升级 / 卸载 (Alpine + OpenRC)。所有设置都用命令行参数传入, 装完即用。
#
# 最常用 (root), 不带参数就进入问答向导 (端口、来源 IP、是否启用 HTTPS):
#   wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | sh
#
# 想一条命令搞定 (跳过问答):
#   wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | \
#     sh -s -- --port 8080 --allow 203.0.113.7
#
# 用 sh install.sh --help 查看全部参数。
set -eu

REPO_DEFAULT="admintors/hubproxy-lite"

usage() {
cat <<'EOF'
用法: sh install.sh [参数]
      不带任何参数 = 进入问答向导 (端口、来源 IP、是否启用 HTTPS)

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
  --email 邮箱         Let's Encrypt 账户邮箱 (可选, 建议填, 用于到期提醒)
  --cert-dir DIR       证书保存目录, 默认 /var/lib/hubproxy-lite
  --acme-staging       使用 Let's Encrypt 测试环境 (证书不受信任, 但不占正式限额; 首次建议先用)

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
DOMAIN=""; EMAIL=""; DNS=""; CF_TOKEN=""; CF_ZONE=""; DNS_HOOK=""; CERT_DIR=""; STAGING=0
BIN=""; REPO="${HP_REPO:-$REPO_DEFAULT}"; TAG="${HP_VERSION:-latest}"
DRY=0; UNINSTALL=0; HAVE_CFG=0
# 只有 HTTPS 相关参数被显式给出时才会记下来 (用于校验)
HTTPS_ARGS=""

while [ $# -gt 0 ]; do
  k="$1"; v=""; hasv=0
  case "$k" in --*=*) v="${k#*=}"; k="${k%%=*}"; hasv=1 ;; esac
  case "$k" in
    -h|--help) usage; exit 0 ;;
    --open)      OPEN=1; HAVE_CFG=1 ;;
    --trust-proxy) TRUST=1; HAVE_CFG=1 ;;
    --log-access)  LOGACC=1; HAVE_CFG=1 ;;
    --acme-staging) STAGING=1; HTTPS_ARGS="$HTTPS_ARGS --acme-staging"; HAVE_CFG=1 ;;
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
        *) HAVE_CFG=1 ;;
      esac
      ;;
    *) die "未知参数: $k  (sh install.sh --help 查看用法)" ;;
  esac
  shift
done

# 环境变量传 token 更安全 (不进 ps); 命令行给了就用命令行的
[ -n "$CF_TOKEN" ] || CF_TOKEN="${HP_CF_TOKEN:-}"

# ---------------------------------------------------------------- 参数校验 (改动系统之前)
# 值会被写进 shell 配置文件, 禁止可能破坏引号的字符
safe() { # 名称 值
  case "$2" in
    *[\"\$\`\\\ ]*|*'
'*) die "$1 的值里不能含有空格、引号、\$、反引号或反斜杠: $2" ;;
  esac
}
is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
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
if [ -n "$DOMAIN" ]; then
  DOMAIN=$(printf %s "$DOMAIN" | tr 'A-Z' 'a-z' | sed 's/\.$//')
  ok_domain "$DOMAIN" || die "--domain 必须是域名 (不能是 IP, 不能含 *), 例 hub.example.com: $DOMAIN"
fi
if [ -n "$DOMAIN" ]; then
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

if [ "$HAVE_CFG" -eq 1 ] && [ -z "$ALLOW" ] && [ "$OPEN" -eq 0 ]; then
  die "必须指定 --allow 来源IP, 或明确写 --open 表示不限制。
  例: sh install.sh --port $PORT --allow 你的公网IP"
fi
[ -z "$ALLOW" ] || [ "$OPEN" -eq 0 ] || die "--allow 和 --open 不能同时使用"

# 生成配置文件内容 (健康检查要从本机访问, 所以白名单里自动补上回环地址)
# $1 = 1 时 token 打码 (dry-run 显示用)
gen_conf() {
  ac=""
  [ -z "$ALLOW" ] || ac="$ALLOW,127.0.0.1,::1"
  echo "# 由 install.sh 生成。要改配置: 重新运行 install.sh 带上新参数, 或手工编辑后 rc-service hubproxy-lite restart"
  echo "export HP_LISTEN=\"$LISTEN:$PORT\""
  echo "export HP_ALLOW_CIDR=\"$ac\""
  echo "export HP_MAX_STREAMS=\"${MAX_STREAMS:-6}\""
  if [ -n "$DOMAIN" ]; then
    echo "export HP_DOMAIN=\"$DOMAIN\""
    [ -z "$EMAIL" ]    || echo "export HP_EMAIL=\"$EMAIL\""
    echo "export HP_DNS_PROVIDER=\"$DNS\""
    if [ "$DNS" = cloudflare ]; then
      if [ "${1:-0}" -eq 1 ]; then echo "export HP_CF_TOKEN=\"****\""; else echo "export HP_CF_TOKEN=\"$CF_TOKEN\""; fi
      [ -z "$CF_ZONE" ] || echo "export HP_CF_ZONE=\"$CF_ZONE\""
    else
      echo "export HP_DNS_HOOK=\"$DNS_HOOK\""
    fi
    echo "export HP_CERT_DIR=\"$CERT_DIR\""
    [ "$STAGING" -eq 0 ] || echo "export HP_ACME_STAGING=\"true\""
  fi
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
  gen_conf 1
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
  echo "已卸载 (日志 /var/log/hubproxy-lite.log 保留, 证书目录不删除, 可手动删除)"
  exit 0
fi

# ---------------------------------------------------------------- 交互式向导
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

interactive() {
  # 中途按 Ctrl+C 时恢复终端回显
  trap 'stty echo <&3 2>/dev/null || true; echo >&2; exit 130' INT TERM
  echo
  echo "=== hubproxy-lite 安装向导 (直接回车 = 用方括号里的默认值) ==="
  echo
  while :; do
    ask "1/3  监听端口 [$PORT]: " "$PORT"; p=$(clean "$REPLY")
    if ok_port "$p"; then PORT="$p"; break; fi
    echo "     端口必须是 1024-65535 的数字 (服务不以 root 运行, 绑不了 1024 以下)" >&2
  done
  echo
  SSHIP=""
  [ -z "${SSH_CONNECTION:-}" ] || SSHIP="${SSH_CONNECTION%% *}"
  echo "2/3  允许使用的来源 IP: 填会用这个代理的机器的公网 IP, 多个用逗号分隔,"
  echo "     可写网段如 192.168.0.0/16。输入 open 表示不限制 (不推荐)。"
  [ -z "$SSHIP" ] || echo "     检测到你当前 SSH 登录的来源是 $SSHIP; 如果以后用代理的不是这台机器, 请改成它的 IP。"
  while :; do
    if [ -n "$SSHIP" ]; then ask "     来源 IP [$SSHIP]: " "$SSHIP"; else ask "     来源 IP: " ""; fi
    a=$(clean "$REPLY")
    case "$a" in
      "") echo "     必须填写。想不限制请输入 open" >&2; continue ;;
      open|OPEN|Open)
        ask "     确认不限制来源? 任何人都能消耗你的流量 [y/N]: " n
        case "$REPLY" in y|Y|yes|YES) OPEN=1; ALLOW=""; break ;; esac
        continue ;;
    esac
    if ok_allow "$a"; then ALLOW="$a"; OPEN=0; break; fi
    echo "     格式不对: 应为 IP 或 IP/掩码, 多个用英文逗号分隔, 例如 203.0.113.7,192.168.0.0/16" >&2
  done
  echo
  echo "3/3  是否启用 HTTPS? 需要一个域名, 且域名的 DNS 由 Cloudflare 或你自己的脚本管理。"
  echo "     不需要开放 80/443 端口。不启用则用 http。"
  ask "     启用 HTTPS? [y/N]: " n
  case "$REPLY" in
    y|Y|yes|YES)
      while :; do
        ask "     域名 (例 hub.example.com): " ""; d=$(clean "$REPLY" | tr 'A-Z' 'a-z' | sed 's/\.$//')
        if ok_domain "$d"; then DOMAIN="$d"; break; fi
        echo "     不是有效的域名 (不能是 IP, 不能含 *)" >&2
      done
      ask "     邮箱 (可空, 用于到期提醒): " ""; e=$(clean "$REPLY")
      if [ -n "$e" ]; then
        if ok_email "$e"; then EMAIL="$e"; else echo "     邮箱格式不对, 已忽略" >&2; fi
      fi
      ask "     DNS 服务商 [1=Cloudflare 2=自定义脚本] (默认 1): " 1
      case "$REPLY" in
        2)
          DNS=exec
          while :; do
            ask "     脚本绝对路径 (协议见 README): " ""; h=$(clean "$REPLY")
            case "$h" in /*) ;; *) echo "     必须是绝对路径" >&2; continue ;; esac
            if [ -f "$h" ] && [ -x "$h" ]; then DNS_HOOK="$h"; break; fi
            echo "     文件不存在或不可执行" >&2
          done
          ;;
        *)
          DNS=cloudflare
          while :; do
            ask_secret "     Cloudflare API Token (输入不回显): "; t=$(clean "$REPLY")
            if [ -n "$t" ] && ok_token "$t"; then CF_TOKEN="$t"; break; fi
            echo "     Token 为空或含非法字符" >&2
          done
          ;;
      esac
      STAGING=0
      ask "     先用 Let's Encrypt 测试环境? (证书不受信任, 建议首次用) [y/N]: " n
      case "$REPLY" in y|Y|yes|YES) STAGING=1 ;; esac
      ;;
  esac
  echo
  echo "即将安装: 端口 $PORT, 来源 ${ALLOW:-不限制}, HTTPS ${DOMAIN:-关闭}"
  [ -z "$DOMAIN" ] || echo "          域名 $DOMAIN, DNS 验证 $DNS$([ "$STAGING" -eq 1 ] && echo ', 测试环境')"
  ask "确认? [Y/n]: " y
  case "$REPLY" in n|N|no|NO) echo "已取消"; exit 1 ;; esac
  trap - INT TERM
  HAVE_CFG=1
}

# 没有配置参数时: 有终端就问答; 没终端则沿用已有配置, 没有配置就报错
KEEP_CONF=0
if [ "$HAVE_CFG" -eq 0 ]; then
  if [ -f "$CONF" ]; then
    KEEP_CONF=1
    if [ "$TTY" -eq 1 ]; then
      cur=$( ( . "$CONF"; echo "端口 ${HP_LISTEN##*:}, 来源 ${HP_ALLOW_CIDR:-不限制}, HTTPS ${HP_DOMAIN:-关闭}" ) 2>/dev/null | sed 's/,127\.0\.0\.1,::1//' || true )
      echo "检测到已有配置: $cur"
      ask "  [1] 保持现有配置, 只升级程序   [2] 重新配置   (默认 1): " 1
      if [ "$REPLY" = 2 ]; then KEEP_CONF=0; interactive; fi
    fi
  elif [ "$TTY" -eq 1 ]; then
    interactive
  else
    die "没有可用的终端来提问。请带参数运行, 例:
  sh install.sh --port 8080 --allow 你的公网IP
  (sh install.sh --help 查看全部参数)"
  fi
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
OLDPORT=""; OLDDOMAIN=""; OLDCERT=""
if [ -f "$CONF" ]; then
  OLDPORT=$( ( . "$CONF"; echo "${HP_LISTEN##*:}" ) 2>/dev/null || true )
  OLDDOMAIN=$( ( . "$CONF"; echo "${HP_DOMAIN:-}" ) 2>/dev/null || true )
  OLDCERT=$( ( . "$CONF"; echo "${HP_CERT_DIR:-}" ) 2>/dev/null || true )
fi
if [ "$KEEP_CONF" -eq 1 ]; then
  PORT="${OLDPORT:-5000}"
  DOMAIN="$OLDDOMAIN"
  [ -z "$OLDCERT" ] || CERT_DIR="$OLDCERT"
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
  [ ! -f "$CONF" ] || { cp "$CONF" "$CONF.bak"; chmod 0600 "$CONF.bak"; }
  umask 077
  gen_conf 0 > "$CONF"
  chmod 0600 "$CONF"
  umask 022
fi

# 证书目录: 归 hubproxy 用户所有, 仅自己可读写 (里面有证书私钥和 ACME 账户密钥)
if [ -n "$DOMAIN" ]; then
  install -d -m 0700 -o hubproxy -g hubproxy "$CERT_DIR" 2>/dev/null || mkdir -p "$CERT_DIR"
  chown hubproxy:hubproxy "$CERT_DIR" 2>/dev/null || true
  chmod 0700 "$CERT_DIR"
fi

rc-update add hubproxy-lite default >/dev/null
rc-service hubproxy-lite start || echo "警告: rc-service start 返回失败, 继续做健康检查"

# ---------------------------------------------------------------- 健康检查
# HTTPS 模式: 证书可能还在签发 (DNS 等待最长 120 秒), 所以健康检查失败不算安装失败;
# 只要进程在跑就是正常的, 证书会在后台继续申请。
ok=0
if [ -n "$DOMAIN" ]; then
  URL="https://127.0.0.1:$PORT/healthz"
  WGET_OPTS="--no-check-certificate"
else
  URL="http://127.0.0.1:$PORT/healthz"
  WGET_OPTS=""
fi
i=0
while [ "$i" -lt 8 ]; do
  # shellcheck disable=SC2086
  if wget -q $WGET_OPTS -O- "$URL" 2>/dev/null | grep -qx ok; then ok=1; break; fi
  i=$((i+1)); sleep 1
done

echo
if [ "$ok" -eq 1 ]; then
  echo "安装完成, 服务运行中, 监听端口 $PORT"
elif [ -n "$DOMAIN" ] && rc-service hubproxy-lite status 2>/dev/null | grep -q started; then
  echo "服务已启动, HTTPS 证书还在申请中 (DNS 验证需要一两分钟)。"
  echo "  查看进度: tail -n 30 /var/log/hubproxy-lite.log"
  ok=1
else
  echo "服务已安装, 但健康检查没通过。排查:"
  echo "  rc-service hubproxy-lite status"
  echo "  tail -n 30 /var/log/hubproxy-lite.log"
fi

if [ -n "$DOMAIN" ]; then
  cat <<EOF

使用 (域名 $DOMAIN, 证书由 Let's Encrypt 签发, 客户端无需额外信任设置):
  GitHub : wget https://$DOMAIN:$PORT/https://github.com/owner/repo/archive/refs/heads/main.tar.gz
  Docker : /etc/docker/daemon.json 加
           {"registry-mirrors":["https://$DOMAIN:$PORT"]}
  注意: DNS 的 A 记录要指向这台服务器; 防火墙放行 TCP $PORT。
EOF
  [ "$STAGING" -eq 0 ] || echo "  当前为测试环境证书 (--acme-staging), 确认无误后去掉该参数重装。"
else
  cat <<EOF

使用 (把 <IP> 换成这台服务器的地址):
  GitHub : wget http://<IP>:$PORT/https://github.com/owner/repo/archive/refs/heads/main.tar.gz
  Docker : /etc/docker/daemon.json 加
           {"registry-mirrors":["http://<IP>:$PORT"],"insecure-registries":["<IP>:$PORT"]}
  别忘了在宿主机/云厂商防火墙放行 TCP $PORT。
EOF
fi
cat <<EOF
改配置: 重新运行 install.sh 带新参数 (旧配置自动备份); 卸载: sh install.sh --uninstall
EOF
[ "$ok" -eq 1 ] || exit 1
