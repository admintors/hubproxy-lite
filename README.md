# hubproxy-lite

[sky22333/hubproxy](https://github.com/sky22333/hubproxy) 的定向精简版，
目标环境：**0.1 核 CPU / 128MB 内存 / 1GB 硬盘 / Alpine LXC / 无域名、纯 HTTP**。

只依赖 Go 标准库，单个静态二进制，约 6.6MB（原版 21.6MB）。

## 保留 / 砍掉

| 功能 | 状态 | 说明 |
|---|---|---|
| Docker 镜像加速 | 保留 | Docker Hub / ghcr.io / quay.io / gcr.io / registry.k8s.io，可用 `HP_REGISTRIES` 加别的 |
| GitHub 文件加速 | 保留 | release / archive / raw / gist / api / git clone / Hugging Face |
| `.sh` `.ps1` 链接改写 | 保留 | 脚本里的 GitHub 链接自动改成走本代理（最大 10MB） |
| IP 限流 | 保留 | 令牌桶，IPv6 按 /64 |
| 镜像/仓库黑白名单 | 保留 | `HP_ALLOW` / `HP_DENY`，支持 `*` 结尾通配 |
| 上游 SOCKS5/HTTP 代理 | 保留 | `HP_UPSTREAM_PROXY` |
| **来源 IP 白名单** | **新增** | `HP_ALLOW_CIDR`，无域名+公网 IP 场景下最重要的一道防线 |
| **并发流上限** | **新增** | `HP_MAX_STREAMS`，0.1 核下防止多路下载互相拖死 |
| **内存软上限** | **新增** | 自动读 cgroup 限额的 70%，避免 Go GC 在 128MB 里顶爆 |
| Web 前端 (Vue) | 砍掉 | 省掉 Node 构建和约 10MB 内嵌资源 |
| 镜像搜索 / 标签浏览 | 砍掉 | 依赖 Docker Hub 搜索 API，自用不需要 |
| 离线镜像包 (tar 打包) | 砍掉 | 吃 CPU，0.1 核下基本跑不动 |
| gin / go-containerregistry | 砍掉 | 换成标准库反向代理，依赖从 60 多个降到 0 |

## 部署（Alpine LXC）

root 执行这一条，然后按提示回答两个问题（端口、来源 IP），直接回车就用默认值：

```sh
wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | sh
```

```
=== hubproxy-lite 安装向导 (直接回车 = 用方括号里的默认值) ===

1/2  监听端口 [5000]: 8080
2/2  允许使用的来源 IP: ...
     检测到你当前 SSH 登录的来源是 203.0.113.7
     来源 IP [203.0.113.7]:
```

脚本会自动下载对应架构的二进制、校验 SHA256、创建服务用户、写好配置、注册 OpenRC 并启动，最后做健康检查。
输入有误（端口越界、IP 格式不对）会提示并让你重填，不会带着错误配置装下去。

### 不想交互？一条命令带参数

```sh
wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | \
  sh -s -- --port 8080 --allow 203.0.113.7
```

### 全部参数

| 参数 | 说明 |
|---|---|
| `--allow IP,网段` | **必填其一**。只允许这些来源使用，如 `203.0.113.7,192.168.0.0/16`。本机回环会自动加入 |
| `--open` | 明确表示不限制来源（不推荐） |
| `--port N` | 监听端口，默认 5000，范围 1024-65535 |
| `--listen IP` | 监听地址，默认 0.0.0.0 |
| `--public-url URL` | 对外地址，如 `http://203.0.113.10:8080`（启用 HTTPS 时自动为 `https://域名[:端口]`） |
| `--domain 域名` | 启用 HTTPS 并指定域名，见上文「HTTPS」一节 |
| `--email 邮箱` | ACME 账户邮箱（建议填写，用于证书到期提醒） |
| `--dns cloudflare\|exec` | DNS 验证方式（启用 HTTPS 时必填） |
| `--cf-token TOKEN` | Cloudflare API Token（也可用环境变量 `HP_CF_TOKEN`） |
| `--cf-zone ID` | Cloudflare Zone ID（可选，不填会自动查找） |
| `--dns-hook PATH` | exec 模式的脚本绝对路径 |
| `--cert-dir PATH` | 证书目录，默认 `/var/lib/hubproxy-lite` |
| `--acme-staging` | 使用 Let's Encrypt 测试环境 |
| `--max-streams N` | 同时下载数，默认 6 |
| `--rate-limit N` / `--rate-hours H` | 限流，默认 500 次 / 3 小时 |
| `--max-file-mb N` | GitHub 单文件上限，默认 2048 |
| `--image-allow` / `--image-deny` | 镜像黑白名单 |
| `--registries 前缀=主机,...` | 额外 registry |
| `--upstream-proxy URL` | 出站代理 |
| `--trust-proxy` / `--log-access` | 信任 X-Forwarded-For / 记录访问日志 |
| `--bin PATH` | 用本地二进制（服务器连不上 GitHub 时） |
| `--repo 用户/仓库` / `--tag vX.Y.Z` | 指定 Release 来源和版本 |
| `--dry-run` | 只检查参数并打印将生成的配置 |
| `--uninstall` | 卸载 |

参数在改动系统之前就会全部校验：端口范围、IP/CIDR 格式、值里的危险字符都会被拦下；
新端口被占用时会在停掉旧服务之前报错，旧服务不受影响。

### 修改配置 / 升级

- **改配置**：重新运行安装命令。检测到已有配置时会问你"保持现有配置只升级程序"还是"重新配置"；带参数运行则直接按新参数重写。旧配置备份为 `/etc/conf.d/hubproxy-lite.bak`。
- **升级**：重新运行安装命令，选 1（默认）即可，沿用已有配置，只替换二进制。
- **卸载**：`sh install.sh --uninstall`。

### 服务器连不上 GitHub

在别处下载 `install.sh` 和 `hubproxy-lite-linux-<arch>`，scp 到服务器同一目录：

```sh
sh install.sh --port 8080 --allow 203.0.113.7
```

脚本会自动使用同目录下的二进制（本地文件不做 SHA 校验，请自行核对）。

### 自行构建

```sh
sh build.sh            # 需要 Go >= 1.22，产出 dist/hubproxy-lite-linux-{amd64,arm64}
```

## HTTPS（域名 + DNS 验证）

适用于端口 80/443 都不能用的情况。程序内置 ACME 客户端，用 **DNS-01** 验证域名，不需要开放任何入站端口，HTTPS 跑在你选的端口上。证书是 Let's Encrypt 的受信任证书，Docker 客户端不需要 `insecure-registries`。

前提：
1. 一个域名，并把它的 A 记录指向这台服务器的公网 IP（例：`hub.example.com`）。
2. DNS 解析商是 Cloudflare，或者你自己写一个 DNS 脚本（见下）。

**Cloudflare**：创建 API Token，权限只给两条：`Zone : Zone : Read` 和 `Zone : DNS : Edit`，范围限定到该域名所在的 Zone。

```sh
sh install.sh --port 8443 --allow 203.0.113.7 \
  --domain hub.example.com --email you@example.com \
  --dns cloudflare --cf-token <你的Token>
```

安装向导也会问这些问题，Token 输入时不回显。

**自定义 DNS 脚本（exec）**：适用于其它解析商。脚本由程序以参数调用，不经过 shell：

```
hook.sh present <完整域名> <TXT值>    # 写入 _acme-challenge.<域名> 的 TXT 记录
hook.sh cleanup <完整域名> <TXT值>    # 删除该记录
```

```sh
sh install.sh --port 8443 --allow 203.0.113.7 --domain hub.example.com \
  --email you@example.com --dns exec --dns-hook /root/hook.sh
```

**首次使用建议加 `--acme-staging`**，先用 Let's Encrypt 测试环境确认流程没问题，再去掉它重装。测试证书不受浏览器信任，这是正常的。

签发过程：程序启动后写入 TXT 记录，等待解析生效（最多 `HP_DNS_WAIT_SECONDS`，默认 120 秒），通知 CA 验证，签发证书并落盘到 `/var/lib/hubproxy-lite`（私钥权限 0600）。之后每天检查一次，剩余有效期不足 1/3 时自动续期，无需重启。

签发失败时服务不会退出，会按 5 分钟、10 分钟…… 逐步退避重试（封顶 1 小时）。失败原因在日志 `/var/log/hubproxy-lite.log` 里。在证书就绪之前，HTTPS 握手会失败。

Docker 配置（有效证书，不需要 insecure-registries）：

```json
{
  "registry-mirrors": ["https://hub.example.com:8443"]
}
```

## 无域名 / 纯 HTTP 怎么用

**GitHub**（不需要任何额外配置）：

```sh
wget "http://<IP>:5000/https://github.com/owner/repo/releases/download/v1/app.tar.gz"
git clone http://<IP>:5000/https://github.com/owner/repo.git
curl -fsSL http://<IP>:5000/https://raw.githubusercontent.com/owner/repo/main/install.sh | sh
```

**Docker**：Docker 默认只信任 HTTPS 的 registry，纯 HTTP 必须把本机 IP 登记为 insecure。
`/etc/docker/daemon.json`：

```json
{
  "registry-mirrors": ["http://<IP>:5000"],
  "insecure-registries": ["<IP>:5000"]
}
```

然后 `systemctl restart docker`，`docker pull nginx` 就会走代理。其它仓库：
`docker pull <IP>:5000/ghcr.io/owner/image:tag`。

> 注意：`registry-mirrors` 只对 Docker Hub 生效；containerd/k8s 用户请配置 hosts.toml 的 mirror。

## 安全须知

- **务必设置 `HP_ALLOW_CIDR`**。纯 HTTP + 公网 IP + 无域名，意味着任何扫描到端口的人都能消耗你的流量。
  未设置时启动日志会有一行提示。
- 纯 HTTP 下流量不加密。所拉取内容的完整性靠 Docker 的 digest 校验保证，但下载 GitHub 文件时请自行校验哈希。
- GitHub 代理只允许访问固定的 GitHub / Hugging Face 主机，且禁止端口，防止被当作通用 SSRF 跳板。
- 响应为 HTML/XML 一律拒绝，避免把网页经你的 IP 暴露出去。

## 环境变量

一般用 `install.sh` 的参数设置即可，它们会写入 `/etc/conf.d/hubproxy-lite`。
完整的变量清单和注释见 `deploy/hubproxy-lite.confd`（手动部署时参考）。

## 测试

```sh
go vet ./... && go test ./...
```

覆盖：路径穿越、SSRF（内网 / 元数据地址 / 域名欺骗 / 非标准端口）、限流、CIDR、X-Forwarded-For 信任、脚本改写、黑白名单。
这些防线都做过变异验证（故意改坏代码，确认测试能抓到）。
