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

root 执行这一条，按提示逐项回答，直接回车就用方括号里的默认值：

```sh
wget -qO- https://github.com/admintors/hubproxy-lite/releases/latest/download/install.sh | sh
```

向导依次问三件事，**HTTPS 要不要开是明确的一问，不会被跳过**：

```
1/3  监听端口 [5000]:
2/3  来源限制: 填会用这个代理的机器的公网 IP (检测到 SSH 来源会作为默认值)
3/3  HTTPS: 先说明启用/不启用各是什么、需要什么, 再问 "启用 HTTPS? [y/N]"
     选 y 后继续问: 域名 / 邮箱 / DNS 验证方式 (Cloudflare 或自定义脚本) / Token (不回显) / 是否用测试环境
```

选了 HTTPS 之后，**安装前**脚本会用你填的设置真实地写入并删除一条测试 TXT 记录（`_hubproxy-check.<域名>`），
Token 权限不够、脚本跑不起来这类问题当场就暴露，不用等装完再翻日志。验证失败时可以：重新填写 / 忽略继续 / 不启用 HTTPS。

最后会列出完整的执行摘要（端口、来源、HTTPS、域名、DNS 方式、证书环境、文件位置），确认后才动系统。
启用 HTTPS 的话，装完会**等证书签发结果并如实报告**（成功、失败原因、或超时仍在重试），不是只说一句"已启动"。

脚本会自动下载对应架构的二进制、校验 SHA256、创建服务用户、写好配置、注册 OpenRC 并启动。
输入有误（端口越界、IP/域名格式不对）会提示并让你重填，不会带着错误配置装下去。

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
| `--no-https` | 明确关闭 HTTPS，改回纯 HTTP（其它配置保留） |
| `--skip-dns-check` | 跳过安装前的 DNS 写入测试 |
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

重新运行安装命令。检测到已有配置时会先显示当前的端口、来源、HTTPS 状态，再问：

```
[1] 保持现有配置, 只升级程序 (默认)
[2] 只修改 HTTPS (开启 / 关闭 / 更换域名或 DNS 设置), 其它配置不动
[3] 全部重新配置 (端口、来源 IP、HTTPS 全部重新问一遍; 其它高级设置会带过去)
```

- 从 v0.1.0（纯 HTTP）升级并想启用 HTTPS：选 **2**。端口、来源、限流等其它配置原样保留。
- 要关闭 HTTPS：选 2 再回答"继续使用 HTTPS? n"，或命令行 `sh install.sh --no-https`。
- 命令行只带 HTTPS 参数（如 `--domain ...`）且已有配置时，同样只改 HTTPS 那几行。
- 旧配置备份为 `/etc/conf.d/hubproxy-lite.bak`。
- **卸载**：`sh install.sh --uninstall`（证书目录不会删）。

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

**DNS 预检**：`hubproxy-lite check-dns` 读取和服务相同的环境变量，写入再删除一条 `_hubproxy-check.<域名>` TXT 记录（不碰 `_acme-challenge`），失败会给出具体原因。`install.sh` 在安装前自动调用它。

**测试环境**：想反复试验可加 `--acme-staging`（不占 Let's Encrypt 正式限额，但证书不受浏览器和 Docker 信任）。从测试环境切回正式环境时，程序会发现证书的签发环境变了并自动重新申请，不会继续使用测试证书。

**Cloudflare 的 A 记录请设为"仅 DNS"（灰云）**，不要开代理（橙云），否则非标准端口和大文件下载会出问题。

**exec 脚本**以 `hubproxy` 用户身份运行，脚本及其所在目录必须对其它用户可读/可执行（放 `/usr/local/bin` 即可，不要放 `/root`），`install.sh` 会提前检查这一点。

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
