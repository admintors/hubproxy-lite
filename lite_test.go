package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, mut func(*config)) *server {
	t.Helper()
	cfg := &config{
		Listen: "127.0.0.1:0", RateLimit: 500, RatePeriod: 3 * time.Hour,
		MaxStreams: 4, MaxFileBytes: 1 << 30, Stall: 30 * time.Second,
		ManifestCache: true, ExtraRegs: map[string]string{},
	}
	if mut != nil {
		mut(cfg)
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(s *server, path string, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = remote
	r.Host = "10.0.0.5:5000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// ---- SSRF: 文件代理只能去白名单主机, 不能被当成通用代理 ----
func TestFileProxyRejectsNonAllowlistedTargets(t *testing.T) {
	s := testServer(t, nil)
	bad := []string{
		"/https://example.com/x",
		"/http://127.0.0.1:5000/healthz",
		"/http://169.254.169.254/latest/meta-data/",
		"/https://github.com:8080/a/b/raw/main/x",     // 带端口
		"/https://user:pw@github.com/a/b/raw/main/x",  // userinfo
		"/https://github.com.evil.com/a/b/raw/main/x", // 后缀欺骗
		"/https://evilgithub.com/a/b/raw/main/x",      // 前缀欺骗
		"/http://[::1]/x",
		"/https://localhost/x",
		"/ftp://github.com/a/b/raw/main/x",
		"/file:///etc/passwd",
	}
	for _, p := range bad {
		w := get(s, p, "1.2.3.4:1111")
		if w.Code != 403 && w.Code != 400 && w.Code != 404 {
			t.Errorf("%s -> %d, 期望被拒绝(4xx)", p, w.Code)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	s := testServer(t, nil)
	for h, want := range map[string]bool{
		"github.com": true, "raw.githubusercontent.com": true, "codeload.github.com": true,
		"objects.githubusercontent.com": true, "GitHub.com": true,
		"github.com.evil.com": false, "evil.com": false, "127.0.0.1": false,
		// 后缀白名单必须带前导点: 这些是 "把后缀直接拼在别人域名后面" 的欺骗写法
		"evilgithubusercontent.com": false, "xgithubassets.com": false,
		"evilhf.co": false, "nothuggingface.co": false,
		"a.b.githubusercontent.com": true, "x.hf.co": true,
		"localhost": false, "": false, "169.254.169.254": false,
	} {
		if got := s.hostAllowed(h); got != want {
			t.Errorf("hostAllowed(%q)=%v want %v", h, got, want)
		}
	}
}

// ---- Registry 路径: 不允许穿越 / 注入 ----
func TestRegistryNameValidation(t *testing.T) {
	for n, want := range map[string]bool{
		"library/alpine": true, "a/b-c_d.e": true, "x": true,
		"../etc": false, "a/../b": false, "a//b": false, "": false,
		"a b": false, "a?x=1": false, "a#b": false, "A/UPPER": false,
	} {
		if got := validName(n); got != want {
			t.Errorf("validName(%q)=%v want %v", n, got, want)
		}
	}
	for n, want := range map[string]bool{
		"latest": true, "3.20": true, "sha256:" + strings.Repeat("a", 64): true,
		"": false, "../x": false, "a/b": false, "a b": false,
	} {
		if got := validRef(n); got != want {
			t.Errorf("validRef(%q)=%v want %v", n, got, want)
		}
	}
}

func TestRegistryRejectsWrites(t *testing.T) {
	s := testServer(t, nil)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		r := httptest.NewRequest(m, "/v2/library/alpine/blobs/uploads/", nil)
		r.RemoteAddr = "1.2.3.4:1"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 405 && w.Code != 403 {
			t.Errorf("%s -> %d, 期望 405/403", m, w.Code)
		}
	}
}

func TestRegistryTraversalRejected(t *testing.T) {
	s := testServer(t, nil)
	for _, p := range []string{
		"/v2/../../etc/passwd/manifests/x",
		"/v2/library/alpine/../../x/manifests/latest",
		"/v2/library/alpine/blobs/..%2f..%2fx",
		"/v2/%2e%2e/x/manifests/latest",
	} {
		w := get(s, p, "1.2.3.4:1")
		if w.Code < 400 {
			t.Errorf("%s -> %d, 期望被拒绝", p, w.Code)
		}
	}
}

// ---- 访问控制 ----
func TestAllowCIDR(t *testing.T) {
	nets, err := parseCIDRs([]string{"10.0.0.0/8", "192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t, func(c *config) { c.AllowCIDR = nets })
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "192.168.1.5": true, "192.168.1.6": false,
		"8.8.8.8": false, "not-an-ip": false,
	} {
		if got := s.ipAllowed(ip); got != want {
			t.Errorf("ipAllowed(%s)=%v want %v", ip, got, want)
		}
	}
	if w := get(s, "/v2/", "8.8.8.8:1"); w.Code != 403 {
		t.Errorf("非白名单 IP 访问 /v2/ -> %d, 期望 403", w.Code)
	}
	if w := get(s, "/v2/", "10.1.2.3:1"); w.Code != 200 {
		t.Errorf("白名单 IP 访问 /v2/ -> %d, 期望 200", w.Code)
	}
}

func TestClientIPTrustProxy(t *testing.T) {
	mk := func(trust bool) *server { return testServer(t, func(c *config) { c.TrustProxy = trust }) }
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	if got := mk(false).clientIP(r); got != "10.0.0.9" {
		t.Errorf("不信任代理时应取连接 IP, got %s", got)
	}
	if got := mk(true).clientIP(r); got != "6.6.6.6" {
		t.Errorf("信任代理时应取 XFF, got %s", got)
	}
}

func TestAllowDenyLists(t *testing.T) {
	s := testServer(t, func(c *config) { c.Deny = []string{"evil/*", "bad/repo"} })
	for n, want := range map[string]bool{
		"evil/x": false, "bad/repo": false, "bad/other": true, "good/x": true, "EVIL/Y": false,
	} {
		if got := s.permitted(n); got != want {
			t.Errorf("permitted(%q)=%v want %v", n, got, want)
		}
	}
	s2 := testServer(t, func(c *config) { c.Allow = []string{"mine/*"} })
	if !s2.permitted("mine/a") || s2.permitted("other/a") {
		t.Error("白名单模式应只放行 mine/*")
	}
}

// ---- 限流 ----
func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.1.1.1"); !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if ok, retry := l.allow("1.1.1.1"); ok || retry <= 0 {
		t.Errorf("第 4 次应限流并给出 retry, ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.allow("2.2.2.2"); !ok {
		t.Error("不同 IP 互不影响")
	}
	// IPv6 按 /64 归并
	a, b := limiterKey("2001:db8:1:2::1"), limiterKey("2001:db8:1:2:ffff::9")
	if a != b {
		t.Errorf("同一 /64 应归并: %s vs %s", a, b)
	}
	if limiterKey("2001:db8:1:3::1") == a {
		t.Error("不同 /64 不应归并")
	}
}

func TestLimiterMemoryBounded(t *testing.T) {
	l := newLimiter(10, time.Hour)
	for i := 0; i < 200000; i++ {
		l.allow(net.IPv4(byte(i>>16), byte(i>>8), byte(i), 1).String())
	}
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n > 50000 {
		t.Errorf("限流表无上限增长: %d 项", n)
	}
}

// ---- 脚本改写 ----
func TestRewriteScript(t *testing.T) {
	base := "http://10.0.0.5:5000"
	in := "curl -L https://github.com/a/b/releases/download/v1/x.tgz\n" +
		"wget \"https://raw.githubusercontent.com/a/b/main/i.sh\"\n" +
		"echo https://example.com/keep\n" +
		"u=http://10.0.0.5:5000/https://github.com/a/b/x\n"
	out := string(rewriteScript([]byte(in), base))
	for _, w := range []string{
		base + "/https://github.com/a/b/releases/download/v1/x.tgz",
		base + "/https://raw.githubusercontent.com/a/b/main/i.sh",
		"https://example.com/keep",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("改写结果缺少 %q\n%s", w, out)
		}
	}
	if strings.Contains(out, base+"/"+base) {
		t.Errorf("出现重复改写:\n%s", out)
	}
	if got := string(rewriteScript([]byte("echo hi"), base)); got != "echo hi" {
		t.Errorf("无 GitHub URL 的脚本不应变化: %q", got)
	}
}

// ---- 健康检查 & 首页不要求上游 ----
func TestHealthAndIndex(t *testing.T) {
	s := testServer(t, nil)
	if w := get(s, "/healthz", "1.2.3.4:1"); w.Code != 200 {
		t.Errorf("/healthz -> %d", w.Code)
	}
	if w := get(s, "/v2/", "1.2.3.4:1"); w.Code != 200 || w.Header().Get("Docker-Distribution-Api-Version") == "" {
		t.Errorf("/v2/ 应返回 200 且带 API 版本头, got %d", w.Code)
	}
	if w := get(s, "/", "1.2.3.4:1"); w.Code != 200 || !strings.Contains(w.Body.String(), "10.0.0.5:5000") {
		t.Errorf("首页应展示当前访问地址, got %d", w.Code)
	}
}

var _ = http.StatusOK
