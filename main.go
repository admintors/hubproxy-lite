// hubproxy-lite: Docker 镜像 / GitHub 文件加速代理的极简实现。
// 目标环境: 0.1 核 CPU / 128MB 内存 / 无域名、纯 HTTP、跑在 Alpine LXC 里。
// 仅依赖 Go 标准库。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

var version = "dev"

const copyBufSize = 64 << 10

var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBufSize); return &b }}

// ---------------------------------------------------------------- 配置

type config struct {
	Listen        string
	AllowCIDR     []*net.IPNet
	RateLimit     int
	RatePeriod    time.Duration
	MaxStreams    int
	Allow, Deny   []string
	MaxFileBytes  int64
	PublicURL     string
	UpstreamProxy string
	ExtraRegs     map[string]string
	TrustProxy    bool
	LogAccess     bool
	Stall         time.Duration
	ManifestCache bool
}

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q 不是整数", k, v)
	}
	return n, nil
}

func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseCIDRs(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, item := range list {
		if !strings.Contains(item, "/") {
			if strings.Contains(item, ":") {
				item += "/128"
			} else {
				item += "/32"
			}
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("HP_ALLOW_CIDR 里的 %q 无效", item)
		}
		out = append(out, n)
	}
	return out, nil
}

func loadConfig() (*config, error) {
	c := &config{
		Listen:        envStr("HP_LISTEN", "0.0.0.0:5000"),
		Allow:         splitList(os.Getenv("HP_ALLOW")),
		Deny:          splitList(os.Getenv("HP_DENY")),
		PublicURL:     strings.TrimRight(envStr("HP_PUBLIC_URL", ""), "/"),
		UpstreamProxy: envStr("HP_UPSTREAM_PROXY", ""),
		TrustProxy:    envBool("HP_TRUST_PROXY", false),
		LogAccess:     envBool("HP_LOG_ACCESS", false),
		ManifestCache: envBool("HP_MANIFEST_CACHE", true),
		ExtraRegs:     map[string]string{},
	}
	var err error
	if c.AllowCIDR, err = parseCIDRs(splitList(os.Getenv("HP_ALLOW_CIDR"))); err != nil {
		return nil, err
	}
	if c.RateLimit, err = envInt("HP_RATE_LIMIT", 500); err != nil {
		return nil, err
	}
	hours := 3.0
	if v := envStr("HP_RATE_HOURS", ""); v != "" {
		if hours, err = strconv.ParseFloat(v, 64); err != nil || hours <= 0 {
			return nil, errors.New("HP_RATE_HOURS 必须是正数")
		}
	}
	c.RatePeriod = time.Duration(hours * float64(time.Hour))
	if c.MaxStreams, err = envInt("HP_MAX_STREAMS", 12); err != nil || c.MaxStreams < 1 {
		return nil, errors.New("HP_MAX_STREAMS 必须 >= 1")
	}
	mb, err := envInt("HP_MAX_FILE_MB", 2048)
	if err != nil {
		return nil, err
	}
	c.MaxFileBytes = int64(mb) << 20
	stall, err := envInt("HP_STALL_SECONDS", 60)
	if err != nil || stall < 5 {
		return nil, errors.New("HP_STALL_SECONDS 必须 >= 5")
	}
	c.Stall = time.Duration(stall) * time.Second
	// HP_REGISTRIES="harbor.local=harbor.local:443,foo.io=registry.foo.io"
	for _, kv := range splitList(os.Getenv("HP_REGISTRIES")) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("HP_REGISTRIES 项 %q 格式应为 前缀=上游主机", kv)
		}
		c.ExtraRegs[k] = v
	}
	return c, nil
}

// ---------------------------------------------------------------- 运行时调优

func readCgroupMemLimit() int64 {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 && n < 1<<50 {
			return n
		}
	}
	return 0
}

// tuneRuntime: 单核 + 软内存上限。用户显式设置了 GOMAXPROCS/GOMEMLIMIT 时尊重用户。
func tuneRuntime() (memLimit int64) {
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	if os.Getenv("GOMEMLIMIT") != "" {
		return -1
	}
	limit := int64(96 << 20)
	if cg := readCgroupMemLimit(); cg > 0 {
		limit = cg * 70 / 100
		if limit > 192<<20 {
			limit = 192 << 20
		}
		if limit < 24<<20 {
			limit = 24 << 20
		}
	}
	debug.SetMemoryLimit(limit)
	return limit
}

// ---------------------------------------------------------------- 访问控制

func matchList(list []string, name string) bool {
	name = strings.ToLower(name)
	for _, p := range list {
		if p == name {
			return true
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if !strings.Contains(p, "/") && strings.HasPrefix(name, p+"/") { // 只写了 owner
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

func (s *server) permitted(name string) bool {
	if len(s.cfg.Allow) > 0 && !matchList(s.cfg.Allow, name) {
		return false
	}
	return !matchList(s.cfg.Deny, name)
}

func (s *server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.cfg.TrustProxy {
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
			if x := r.Header.Get("X-Real-IP"); x != "" {
				return strings.TrimSpace(x)
			}
			if x := r.Header.Get("X-Forwarded-For"); x != "" {
				first, _, _ := strings.Cut(x, ",")
				return strings.TrimSpace(first)
			}
		}
	}
	return host
}

func (s *server) ipAllowed(ipStr string) bool {
	if len(s.cfg.AllowCIDR) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.cfg.AllowCIDR {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 服务器

type server struct {
	cfg     *config
	client  *http.Client
	scheme  string            // 上游协议, 恒为 https
	hubHost string            // Docker Hub 上游
	regs    map[string]string // 镜像名前缀 -> 上游主机
	hosts   map[string]bool   // 允许代理的文件站点
	tokens  *tokenCache
	mc      *manifestCache
	lim     *limiter
	sem     chan struct{}
	start   time.Time
}

func newServer(cfg *config) (*server, error) {
	proxyFn := http.ProxyFromEnvironment
	if cfg.UpstreamProxy != "" {
		u, err := parseProxyURL(cfg.UpstreamProxy)
		if err != nil {
			return nil, err
		}
		proxyFn = http.ProxyURL(u)
	}
	tr := &http.Transport{
		Proxy:                 proxyFn,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true, // 不替客户端解压, 省 CPU; 脚本改写时自行处理
		ReadBufferSize:        32 << 10,
		WriteBufferSize:       4 << 10,
		// 不设 ForceAttemptHTTP2: 走 HTTP/1.1, 避免 h2 每流 4MB 的接收窗口吃内存
	}
	s := &server{
		cfg: cfg,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		scheme:  "https",
		hubHost: "registry-1.docker.io",
		regs: map[string]string{
			"docker.io":         "registry-1.docker.io",
			"ghcr.io":           "ghcr.io",
			"gcr.io":            "gcr.io",
			"quay.io":           "quay.io",
			"registry.k8s.io":   "registry.k8s.io",
			"k8s.gcr.io":        "registry.k8s.io",
			"mcr.microsoft.com": "mcr.microsoft.com",
		},
		hosts:  defaultFileHosts(),
		tokens: newTokenCache(),
		mc:     newManifestCache(),
		lim:    newLimiter(cfg.RateLimit, cfg.RatePeriod),
		sem:    make(chan struct{}, cfg.MaxStreams),
		start:  time.Now(),
	}
	for k, v := range cfg.ExtraRegs {
		s.regs[k] = v
	}
	return s, nil
}

func (s *server) acquire(ctx context.Context) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
	}
	t := time.NewTimer(3 * time.Second)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

func (s *server) release() { <-s.sem }

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int)           { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.ipAllowed(ip) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.cfg.LogAccess {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		w = sw
		t0 := time.Now()
		defer func() {
			log.Printf("%s %s %s -> %d (%s)", ip, r.Method, r.URL.Path, sw.code, time.Since(t0).Round(time.Millisecond))
		}()
	}

	p := r.URL.Path
	switch {
	case p == "/healthz":
		io.WriteString(w, "ok\n")
	case p == "/":
		s.handleIndex(w, r)
	case p == "/v2" || strings.HasPrefix(p, "/v2/"):
		s.handleRegistry(w, r, ip)
	case p == "/favicon.ico":
		http.NotFound(w, r)
	default:
		s.handleFile(w, r, ip)
	}
}

// ---------------------------------------------------------------- 公共: 头处理 / 流式拷贝

var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func cleanReqHeaders(src http.Header) http.Header {
	h := make(http.Header, len(src)+1)
	for k, v := range src {
		h[k] = append([]string(nil), v...)
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	for _, k := range []string{"Host", "Cookie", "Content-Length", "Forwarded", "X-Forwarded-For",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Cf-Connecting-Ip"} {
		h.Del(k)
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", "hubproxy-lite/"+version)
	}
	return h
}

func writeRespHeaders(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for k, v := range resp.Header {
		dst[k] = append([]string(nil), v...)
	}
	for _, k := range hopHeaders {
		dst.Del(k)
	}
	for _, k := range []string{"Set-Cookie", "Content-Security-Policy", "Strict-Transport-Security", "Referrer-Policy"} {
		dst.Del(k)
	}
}

type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func newIdleReader(r io.Reader, d time.Duration, cancel context.CancelFunc) *idleReader {
	return &idleReader{r: r, t: time.AfterFunc(d, cancel), d: d}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	i.t.Reset(i.d)
	return n, err
}

func (i *idleReader) Stop() { i.t.Stop() }

// stream 把上游 body 流式写给客户端: 64KB 池化缓冲, 不做任何哈希/解压;
// 上游或客户端任一方停滞超过 Stall 就中断, 避免慢连接长期占着并发名额。
func (s *server) stream(w http.ResponseWriter, body io.Reader, cancel context.CancelFunc) {
	ir := newIdleReader(body, s.cfg.Stall, cancel)
	defer ir.Stop()
	rc := http.NewResponseController(w)
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, err := ir.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Stall))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// do 发起上游请求并在服务端跟随重定向 (CDN 直链对国内客户端往往不可达)。
// 跨主机重定向时丢弃 Authorization, 防止 token 泄露给 CDN。
func (s *server) do(ctx context.Context, method, rawurl string, hdr http.Header, body io.Reader, clen int64) (*http.Response, error) {
	for i := 0; i <= 8; i++ {
		req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
		if err != nil {
			return nil, err
		}
		req.Header = hdr
		if body != nil {
			req.ContentLength = clen
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		if body != nil || resp.StatusCode < 300 || resp.StatusCode > 399 || resp.StatusCode == http.StatusNotModified {
			return resp, nil
		}
		loc, err := resp.Location()
		if err != nil {
			return resp, nil
		}
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		resp.Body.Close()
		if loc.Scheme != s.scheme {
			return nil, fmt.Errorf("拒绝重定向到 %s://", loc.Scheme)
		}
		if loc.Host != req.URL.Host {
			hdr = hdr.Clone()
			hdr.Del("Authorization")
		}
		rawurl = loc.String()
	}
	return nil, errors.New("重定向次数过多")
}

// ---------------------------------------------------------------- 首页 (纯文本, 自动填入当前访问地址)

func (s *server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" || !s.cfg.TrustProxy {
		host = r.Host
	}
	return scheme + "://" + host
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	hp := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var dockerCfg string
	if strings.HasPrefix(base, "https://") {
		dockerCfg = fmt.Sprintf(`[Docker] /etc/docker/daemon.json (HTTPS 有可信证书, 不需要 insecure-registries):
  {"registry-mirrors": ["%s"]}`, base)
	} else {
		dockerCfg = fmt.Sprintf(`[Docker] /etc/docker/daemon.json (无域名/HTTP 必须同时配置 insecure-registries):
  {"registry-mirrors": ["%s"], "insecure-registries": ["%s"]}`, base, hp)
	}
	fmt.Fprintf(w, `hubproxy-lite %s  (运行 %s)

%s
  然后 systemctl restart docker, 之后 docker pull nginx 即走本代理。
  其它仓库:  docker pull %s/ghcr.io/owner/image:tag
             docker pull %s/quay.io/owner/image:tag

[GitHub]
  wget "%s/https://github.com/owner/repo/releases/download/v1/app.tar.gz"
  git clone %s/https://github.com/owner/repo.git
  curl -fsSL %s/https://raw.githubusercontent.com/owner/repo/main/install.sh | sh

健康检查: %s/healthz
`, version, time.Since(s.start).Round(time.Second), dockerCfg, hp, hp, base, base, base, base)
}
