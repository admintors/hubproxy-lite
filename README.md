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

在能联网的机器上构建（或直接用 `dist/` 里已有的二进制）：

```sh
sh build.sh            # 需要 Go >= 1.22，产出 dist/hubproxy-lite-linux-{amd64,arm64}
```

把 `dist/` 和 `deploy/` 拷到 Alpine 里，root 执行：

```sh
sh deploy/install.sh
vi /etc/conf.d/hubproxy-lite     # 至少设置 HP_ALLOW_CIDR
rc-service hubproxy-lite restart
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

见 `deploy/hubproxy-lite.confd`，每项都有注释。

## 测试

```sh
go vet ./... && go test ./...
```

覆盖：路径穿越、SSRF（内网 / 元数据地址 / 域名欺骗 / 非标准端口）、限流、CIDR、X-Forwarded-For 信任、脚本改写、黑白名单。
这些防线都做过变异验证（故意改坏代码，确认测试能抓到）。
