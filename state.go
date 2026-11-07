package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 上游 token 缓存

type tokEntry struct {
	tok string
	exp time.Time
}

type authInfo struct {
	realm, service string
	none           bool // 上游无需认证, 或只支持我们无法匿名完成的认证方式
}

type tokenCache struct {
	mu   sync.Mutex
	tok  map[string]tokEntry
	auth map[string]authInfo
}

func newTokenCache() *tokenCache {
	return &tokenCache{
		tok: map[string]tokEntry{},
		auth: map[string]authInfo{
			// Docker Hub 的认证端点固定, 预置以省一次往返
			"registry-1.docker.io": {realm: "https://auth.docker.io/token", service: "registry.docker.io"},
		},
	}
}

func (t *tokenCache) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, e := range t.tok {
		if now.After(e.exp) {
			delete(t.tok, k)
		}
	}
	if len(t.tok) > 512 {
		t.tok = map[string]tokEntry{}
	}
}

// ---------------------------------------------------------------- manifest 缓存 (严格限量)

const (
	mcMaxBytes = 4 << 20   // 总量上限 4MB
	mcMaxItem  = 256 << 10 // 单条上限 256KB, 超过就不缓存直接透传
)

type mcItem struct {
	body []byte
	hdr  http.Header
	exp  time.Time
}

type manifestCache struct {
	mu   sync.Mutex
	m    map[string]*mcItem
	size int
}

func newManifestCache() *manifestCache { return &manifestCache{m: map[string]*mcItem{}} }

func (c *manifestCache) get(key string) *mcItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	it := c.m[key]
	if it == nil {
		return nil
	}
	if time.Now().After(it.exp) {
		c.size -= len(it.body)
		delete(c.m, key)
		return nil
	}
	return it
}

func (c *manifestCache) set(key string, it *mcItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.m[key]; old != nil {
		c.size -= len(old.body)
	}
	if c.size+len(it.body) > mcMaxBytes {
		c.sweepLocked()
		if c.size+len(it.body) > mcMaxBytes { // 仍然放不下: 直接清空, 简单可靠
			c.m = map[string]*mcItem{}
			c.size = 0
		}
	}
	c.m[key] = it
	c.size += len(it.body)
}

func (c *manifestCache) sweepLocked() {
	now := time.Now()
	for k, it := range c.m {
		if now.After(it.exp) {
			c.size -= len(it.body)
			delete(c.m, k)
		}
	}
}

func (c *manifestCache) sweep() {
	c.mu.Lock()
	c.sweepLocked()
	c.mu.Unlock()
}

func manifestTTL(ref string) time.Duration {
	switch ref {
	case "latest", "main", "master", "dev", "develop", "nightly", "edge":
		return 5 * time.Minute
	}
	if len(ref) > 7 && ref[:7] == "sha256:" {
		return time.Hour // 内容寻址, 不会变; 取 1 小时只是为了及时释放内存
	}
	return 15 * time.Minute
}

// ---------------------------------------------------------------- 按 IP 令牌桶限流

const limiterMaxIPs = 4096

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu       sync.Mutex
	m        map[string]*bucket
	rate     float64 // 令牌/秒
	burst    float64
	disabled bool
}

func newLimiter(limit int, period time.Duration) *limiter {
	if limit <= 0 {
		return &limiter{disabled: true}
	}
	return &limiter{
		m:     map[string]*bucket{},
		rate:  float64(limit) / period.Seconds(),
		burst: float64(limit),
	}
}

func limiterKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if p.To4() != nil {
		return p.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64" // IPv6 按 /64 归并
}

// allow 返回是否放行; 被拒绝时给出建议的等待时间。
func (l *limiter) allow(ip string) (bool, time.Duration) {
	if l.disabled {
		return true, 0
	}
	key := limiterKey(ip)
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= limiterMaxIPs {
			l.sweepLocked(now)
			if len(l.m) >= limiterMaxIPs {
				l.m = map[string]*bucket{}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func (l *limiter) sweepLocked(now time.Time) {
	for k, b := range l.m {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst { // 已回满 = 闲置
			delete(l.m, k)
		}
	}
}

func (l *limiter) sweep() {
	if l.disabled {
		return
	}
	l.mu.Lock()
	l.sweepLocked(time.Now())
	l.mu.Unlock()
}

// ---------------------------------------------------------------- 杂项

func parseProxyURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("HP_UPSTREAM_PROXY 无效: %v", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	}
	return nil, fmt.Errorf("HP_UPSTREAM_PROXY 不支持的协议 %q (可用 http/https/socks5/socks5h)", u.Scheme)
}

func defaultFileHosts() map[string]bool {
	m := map[string]bool{}
	for _, h := range []string{
		"github.com", "raw.githubusercontent.com", "raw.github.com",
		"gist.github.com", "gist.githubusercontent.com", "api.github.com",
		"codeload.github.com", "objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"github.githubassets.com", "opengraph.githubassets.com",
		"huggingface.co", "hf.co", "cdn-lfs.hf.co", "cdn-lfs-us-1.hf.co",
	} {
		m[h] = true
	}
	return m
}
