package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 证书工具

func mkCert(t *testing.T, dns string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dns},
		DNSNames:     []string{dns},
		NotBefore:    notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func newTestMgr(t *testing.T, domain string) *certManager {
	t.Helper()
	return newCertManager(&tlsConfig{Domain: domain, CertDir: t.TempDir()})
}

func writePair(t *testing.T, m *certManager, c, k []byte) {
	t.Helper()
	if err := os.WriteFile(m.certPath(), c, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.keyPath(), k, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- 续期判断

func TestNeedsRenewal(t *testing.T) {
	now := time.Now()
	mk := func(life, age time.Duration) *x509.Certificate {
		return &x509.Certificate{NotBefore: now.Add(-age), NotAfter: now.Add(life - age)}
	}
	day := 24 * time.Hour
	cases := []struct {
		name string
		c    *x509.Certificate
		want bool
	}{
		{"没有证书", nil, true},
		{"90天证书刚签", mk(90*day, 0), false},
		{"90天证书过了59天(剩31天)", mk(90*day, 59*day), false},
		{"90天证书过了61天(剩29天)", mk(90*day, 61*day), true},
		{"已过期", mk(90*day, 100*day), true},
		{"6天证书刚签", mk(6*day, 0), false},
		{"6天证书过了3天(剩3天,>2天)", mk(6*day, 3*day), false},
		{"6天证书过了5天(剩1天,<2天)", mk(6*day, 5*day), true},
	}
	for _, c := range cases {
		if got := needsRenewal(c.c, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 加载校验

func TestLoadCert(t *testing.T) {
	now := time.Now()
	t.Run("有效证书可加载并被 TLS 层使用", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		c, k := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
		writePair(t, m, c, k)
		if err := m.load(); err != nil {
			t.Fatal(err)
		}
		got, err := m.getCertificate(nil)
		if err != nil || got == nil {
			t.Fatalf("getCertificate: %v", err)
		}
	})
	t.Run("域名不匹配被拒绝", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		c, k := mkCert(t, "other.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
		writePair(t, m, c, k)
		if err := m.load(); err == nil {
			t.Fatal("域名不匹配的证书不应被加载")
		}
	})
	t.Run("过期证书被拒绝", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		c, k := mkCert(t, "a.example.com", now.Add(-48*time.Hour), now.Add(-time.Hour))
		writePair(t, m, c, k)
		if err := m.load(); err == nil {
			t.Fatal("过期证书不应被加载")
		}
	})
	t.Run("证书和私钥不配对被拒绝", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		c, _ := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
		_, k2 := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
		writePair(t, m, c, k2)
		if err := m.load(); err == nil {
			t.Fatal("不配对的证书/私钥不应被加载")
		}
	})
	t.Run("加载失败不破坏内存里已有的好证书", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		c, k := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
		writePair(t, m, c, k)
		if err := m.load(); err != nil {
			t.Fatal(err)
		}
		before, _ := m.getCertificate(nil)
		_ = os.WriteFile(m.certPath(), []byte("garbage"), 0o644)
		if err := m.load(); err == nil {
			t.Fatal("损坏的证书文件应加载失败")
		}
		after, err := m.getCertificate(nil)
		if err != nil || after != before {
			t.Fatal("加载失败后原证书应保持可用")
		}
	})
	t.Run("没有证书时握手返回错误而不是 panic", func(t *testing.T) {
		m := newTestMgr(t, "a.example.com")
		if _, err := m.getCertificate(nil); err == nil {
			t.Fatal("应报错")
		}
	})
}

// 热更新: 同一个 tls.Config 在换证后, 新握手拿到新证书, 不需要重启
func TestHotReload(t *testing.T) {
	now := time.Now()
	m := newTestMgr(t, "a.example.com")
	c1, k1 := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(48*time.Hour))
	writePair(t, m, c1, k1)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	cfg := m.tlsConfig()
	a, _ := cfg.GetCertificate(&tls.ClientHelloInfo{})
	c2, k2 := mkCert(t, "a.example.com", now.Add(-time.Hour), now.Add(96*time.Hour))
	writePair(t, m, c2, k2)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	b, _ := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if a == b || a.Leaf.SerialNumber.Cmp(b.Leaf.SerialNumber) == 0 {
		t.Fatal("换证后握手仍在使用旧证书")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatal("最低 TLS 版本应为 1.2")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.key")
	if err := writeFileAtomic(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限应为 0600, 实际 %v", st.Mode().Perm())
	}
	// 覆盖写不留临时文件
	if err := writeFileAtomic(p, []byte("secret2"), 0o600); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(d)
	if len(ents) != 1 {
		t.Fatalf("目录里应只有 1 个文件, 实际 %d", len(ents))
	}
}

// ---------------------------------------------------------------- 域名校验

func TestValidDomain(t *testing.T) {
	ok := []string{"proxy.example.com", "a-b.example.co.uk", "x1.y2.example.org"}
	bad := []string{"", "1.2.3.4", "::1", "*.example.com", "localhost", "bad_name.com", "-a.com", "a..com",
		"a.com/path", "a.com:443", "a b.com", "example.c", strings.Repeat("a", 64) + ".com"}
	for _, d := range ok {
		if err := validDomain(d); err != nil {
			t.Errorf("%q 应通过: %v", d, err)
		}
	}
	for _, d := range bad {
		if err := validDomain(d); err == nil {
			t.Errorf("%q 应被拒绝", d)
		}
	}
	if err := validDomain("*.example.com"); err == nil || !strings.Contains(err.Error(), "通配符") {
		t.Errorf("通配符应以通配符原因被拒绝, 实际: %v", err)
	}
	if err := validDomain("1.2.3.4"); err == nil || !strings.Contains(err.Error(), "不能是 IP") {
		t.Errorf("IP 应以 IP 原因被拒绝, 实际: %v", err)
	}
}

// ---------------------------------------------------------------- Cloudflare (本地假服务器)

type fakeCF struct {
	mu      sync.Mutex
	records map[string]map[string]string // id -> {name, content}
	zones   map[string]string            // name -> id
	calls   []string
	token   string
	nextID  int
}

func newFakeCF(token string, zones map[string]string) (*fakeCF, *httptest.Server) {
	f := &fakeCF{records: map[string]map[string]string{}, zones: zones, token: token}
	return f, httptest.NewServer(f)
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	reply := func(code int, ok bool, result any, msg string) {
		w.WriteHeader(code)
		out := map[string]any{"success": ok, "result": result}
		if !ok {
			out["errors"] = []map[string]any{{"code": 9109, "message": msg}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		reply(403, false, nil, "Invalid access token")
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/zones":
		name := r.URL.Query().Get("name")
		if id, ok := f.zones[name]; ok {
			reply(200, true, []map[string]string{{"id": id, "name": name}}, "")
		} else {
			reply(200, true, []map[string]string{}, "")
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/dns_records"):
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["type"] != "TXT" {
			reply(400, false, nil, "only TXT")
			return
		}
		f.nextID++
		id := "rec" + string(rune('0'+f.nextID))
		f.records[id] = map[string]string{"name": b["name"].(string), "content": b["content"].(string)}
		reply(200, true, map[string]string{"id": id}, "")
	case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/dns_records/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		delete(f.records, id)
		reply(200, true, map[string]string{"id": id}, "")
	default:
		reply(404, false, nil, "not found")
	}
}

func TestCloudflareProvider(t *testing.T) {
	ctx := context.Background()
	t.Run("按域名逐级向上找 zone, 写入与清理 TXT", func(t *testing.T) {
		f, srv := newFakeCF("tok", map[string]string{"example.com": "Z1"})
		defer srv.Close()
		cf := newCloudflareDNS("tok", "")
		cf.base = srv.URL
		fq := "_acme-challenge.proxy.example.com"
		if err := cf.Present(ctx, fq, "v1"); err != nil {
			t.Fatal(err)
		}
		if len(f.records) != 1 {
			t.Fatalf("应有 1 条记录, 实际 %d", len(f.records))
		}
		for _, r := range f.records {
			if r["name"] != fq || r["content"] != "v1" {
				t.Fatalf("记录内容不对: %v", r)
			}
		}
		if err := cf.Cleanup(ctx, fq, "v1"); err != nil {
			t.Fatal(err)
		}
		if len(f.records) != 0 {
			t.Fatal("清理后不应有残留记录")
		}
	})
	t.Run("指定 zone id 时不查询 zone", func(t *testing.T) {
		f, srv := newFakeCF("tok", map[string]string{})
		defer srv.Close()
		cf := newCloudflareDNS("tok", "ZFIXED")
		cf.base = srv.URL
		if err := cf.Present(ctx, "_acme-challenge.a.example.com", "v"); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.calls {
			if strings.HasPrefix(c, "GET /zones") {
				t.Fatalf("不应查询 zone: %v", f.calls)
			}
		}
	})
	t.Run("Token 错误给出权限提示", func(t *testing.T) {
		_, srv := newFakeCF("right", map[string]string{"example.com": "Z1"})
		defer srv.Close()
		cf := newCloudflareDNS("wrong", "")
		cf.base = srv.URL
		err := cf.Present(ctx, "_acme-challenge.a.example.com", "v")
		if err == nil || !strings.Contains(err.Error(), "Token") {
			t.Fatalf("应提示检查 Token 权限, 实际: %v", err)
		}
	})
	t.Run("域名不在账户下给出明确错误", func(t *testing.T) {
		_, srv := newFakeCF("tok", map[string]string{"other.org": "Z9"})
		defer srv.Close()
		cf := newCloudflareDNS("tok", "")
		cf.base = srv.URL
		err := cf.Present(ctx, "_acme-challenge.a.example.com", "v")
		if err == nil || !strings.Contains(err.Error(), "找不到") {
			t.Fatalf("应提示找不到 zone, 实际: %v", err)
		}
	})
	t.Run("同名多个值时只清理自己那条", func(t *testing.T) {
		f, srv := newFakeCF("tok", map[string]string{"example.com": "Z1"})
		defer srv.Close()
		cf := newCloudflareDNS("tok", "")
		cf.base = srv.URL
		fq := "_acme-challenge.a.example.com"
		_ = cf.Present(ctx, fq, "v1")
		_ = cf.Present(ctx, fq, "v2")
		_ = cf.Cleanup(ctx, fq, "v1")
		if len(f.records) != 1 {
			t.Fatalf("应剩 1 条, 实际 %d", len(f.records))
		}
		for _, r := range f.records {
			if r["content"] != "v2" {
				t.Fatal("清理了不该清理的记录")
			}
		}
	})
}

// ---------------------------------------------------------------- 构造参数

func TestBuildDNSProviderErrors(t *testing.T) {
	cases := []struct{ p, tok, zone, hook string }{
		{"", "", "", ""},
		{"cloudflare", "", "", ""},
		{"exec", "", "", ""},
		{"exec", "", "", "/nonexistent/hook"},
		{"route53", "", "", ""},
	}
	for _, c := range cases {
		if _, err := buildDNSProvider(c.p, c.tok, c.zone, c.hook); err == nil {
			t.Errorf("%+v 应报错", c)
		}
	}
	// 没有执行权限的脚本
	f := filepath.Join(t.TempDir(), "h.sh")
	_ = os.WriteFile(f, []byte("#!/bin/sh\n"), 0o644)
	if _, err := buildDNSProvider("exec", "", "", f); err == nil {
		t.Error("无执行权限的脚本应报错")
	}
	_ = os.Chmod(f, 0o755)
	if _, err := buildDNSProvider("exec", "", "", f); err != nil {
		t.Errorf("可执行脚本应通过: %v", err)
	}
}

// exec 钩子: 参数经 argv 传递, 不经过 shell, 注入字符不会被执行
func TestExecHookNoShellInjection(t *testing.T) {
	d := t.TempDir()
	marker := filepath.Join(d, "pwned")
	out := filepath.Join(d, "args")
	script := filepath.Join(d, "h.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$3\" > "+out+"\n"), 0o755)
	e := &execDNS{script: script}
	evil := "x\"; touch " + marker + "; echo \""
	if err := e.Present(context.Background(), "_acme-challenge.a.com", evil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("参数被当成 shell 命令执行了")
	}
	got, _ := os.ReadFile(out)
	if string(got) != "present|_acme-challenge.a.com|"+evil {
		t.Fatalf("参数传递不对: %q", got)
	}
}

func TestExecHookFailureReportsOutput(t *testing.T) {
	script := filepath.Join(t.TempDir(), "h.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\necho 'api key invalid' >&2\nexit 2\n"), 0o755)
	err := (&execDNS{script: script}).Present(context.Background(), "_acme-challenge.a.com", "v")
	if err == nil || !strings.Contains(err.Error(), "api key invalid") {
		t.Fatalf("错误里应带上脚本输出, 实际: %v", err)
	}
}

// ---------------------------------------------------------------- 续期循环

// 续期循环: 证书进入续期窗口后, 不重启就能换上新证书; 失败会退避重试; 成功后不再重复申请。
func TestRenewalLoopReplacesCertWithoutRestart(t *testing.T) {
	const dom = "r.example.com"
	m := newTestMgr(t, dom)
	m.interval = 20 * time.Millisecond
	m.backoff = [2]time.Duration{10 * time.Millisecond, 40 * time.Millisecond}

	// 旧证书: 总寿命 90h, 只剩 10h (< 1/3), 应触发续期
	now := time.Now()
	c0, k0 := mkCert(t, dom, now.Add(-80*time.Hour), now.Add(10*time.Hour))
	writePair(t, m, c0, k0)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []time.Time
	m.issue = func(ctx context.Context) ([]byte, []byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		if len(calls) <= 2 { // 前两次失败, 验证退避
			return nil, nil, context.DeadlineExceeded
		}
		n := time.Now()
		c, k := mkCert(t, dom, n.Add(-time.Minute), n.Add(90*24*time.Hour))
		return c, k, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()

	cfg := m.tlsConfig()
	old, _ := cfg.GetCertificate(&tls.ClientHelloInfo{})
	deadline := time.Now().Add(5 * time.Second)
	var cur *tls.Certificate
	for time.Now().Before(deadline) {
		cur, _ = cfg.GetCertificate(&tls.ClientHelloInfo{})
		if cur.Leaf.SerialNumber.Cmp(old.Leaf.SerialNumber) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cur.Leaf.SerialNumber.Cmp(old.Leaf.SerialNumber) == 0 {
		t.Fatal("续期循环没有换上新证书")
	}
	time.Sleep(150 * time.Millisecond) // 新证书很新, 之后不该再申请
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 run 没有退出")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("申请次数 = %d, 期望 3 (失败2次+成功1次, 成功后不再申请)", len(calls))
	}
	if calls[1].Sub(calls[0]) < 10*time.Millisecond || calls[2].Sub(calls[1]) < 20*time.Millisecond {
		t.Fatalf("退避间隔不对: %v %v", calls[1].Sub(calls[0]), calls[2].Sub(calls[1]))
	}
	// 成功后磁盘上也是新证书, 私钥权限 0600
	st, err := os.Stat(m.keyPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限不对: %v %v", st, err)
	}
}

// 申请返回了与域名不符的证书时, 不能覆盖磁盘上现有的好证书。
func TestObtainRejectsWrongCertKeepsOld(t *testing.T) {
	const dom = "k.example.com"
	m := newTestMgr(t, dom)
	now := time.Now()
	c0, k0 := mkCert(t, dom, now.Add(-time.Hour), now.Add(24*time.Hour))
	writePair(t, m, c0, k0)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	before := m.current().SerialNumber
	m.issue = func(context.Context) ([]byte, []byte, error) {
		c, k := mkCert(t, "evil.example.com", now.Add(-time.Hour), now.Add(24*time.Hour))
		return c, k, nil
	}
	if err := m.obtain(context.Background()); err == nil {
		t.Fatal("域名不符的证书应被拒绝")
	}
	if m.current().SerialNumber.Cmp(before) != 0 {
		t.Fatal("坏证书替换了内存里的好证书")
	}
	got, _ := os.ReadFile(m.certPath())
	if string(got) != string(c0) {
		t.Fatal("坏证书覆盖了磁盘上的好证书")
	}
}
