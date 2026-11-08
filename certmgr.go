package main

// 证书管理: 读盘 / 热加载 / 到期前自动续期 / 失败退避。
// 服务在没有证书或申请失败时也保持运行并定时重试, 绝不能退出:
// OpenRC 会无限重启, 反复失败会触发 Let's Encrypt 的失败验证限额。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type tlsConfig struct {
	Domain   string
	Email    string
	DNS      dnsProvider
	CertDir  string
	ACMEURL  string
	Resolver string
	DNSWait  time.Duration
	CAFile   string // 仅测试用: 自定义 ACME 服务器的根证书
}

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func validDomain(d string) error {
	if net.ParseIP(d) != nil {
		return errors.New("HP_DOMAIN 必须是域名, 不能是 IP (IP 证书需要开放 80/443 端口)")
	}
	if strings.Contains(d, "*") {
		return errors.New("HP_DOMAIN 不支持通配符")
	}
	if !hostRe.MatchString(d) {
		return fmt.Errorf("HP_DOMAIN 不是有效的域名: %q", d)
	}
	return nil
}

// loadTLSConfig 在 HP_DOMAIN 为空时返回 nil (表示用纯 HTTP)。
func loadTLSConfig() (*tlsConfig, error) {
	d := strings.ToLower(strings.TrimSuffix(envStr("HP_DOMAIN", ""), "."))
	if d == "" {
		return nil, nil
	}
	if err := validDomain(d); err != nil {
		return nil, err
	}
	dns, err := buildDNSProvider(strings.ToLower(envStr("HP_DNS_PROVIDER", "")),
		envStr("HP_CF_TOKEN", ""), envStr("HP_CF_ZONE", ""), envStr("HP_DNS_HOOK", ""))
	if err != nil {
		return nil, err
	}
	t := &tlsConfig{
		Domain: d, Email: envStr("HP_EMAIL", ""), DNS: dns,
		CertDir:  envStr("HP_CERT_DIR", "/var/lib/hubproxy-lite"),
		ACMEURL:  acmeProdURL,
		Resolver: envStr("HP_DNS_RESOLVER", "1.1.1.1:53"),
		CAFile:   envStr("HP_ACME_CA_FILE", ""),
	}
	if envBool("HP_ACME_STAGING", false) {
		t.ACMEURL = acmeStagingURL
	}
	if u := envStr("HP_ACME_URL", ""); u != "" {
		t.ACMEURL = u
	}
	wait, err := envInt("HP_DNS_WAIT_SECONDS", 120)
	if err != nil || wait < 0 {
		return nil, errors.New("HP_DNS_WAIT_SECONDS 必须是非负整数")
	}
	t.DNSWait = time.Duration(wait) * time.Second
	return t, nil
}

type certManager struct {
	cfg      *tlsConfig
	interval time.Duration    // 检查间隔, 0 表示默认 24h; 仅测试使用
	issue    issueFunc        // nil 表示走真正的 ACME; 仅测试替换
	backoff  [2]time.Duration // 失败退避的 [起点, 上限], 零值表示 5m / 1h; 仅测试使用

	mu   sync.RWMutex
	cert *tls.Certificate
	leaf *x509.Certificate
}

func newCertManager(cfg *tlsConfig) *certManager { return &certManager{cfg: cfg} }

func (m *certManager) certPath() string { return filepath.Join(m.cfg.CertDir, m.cfg.Domain+".crt") }
func (m *certManager) keyPath() string  { return filepath.Join(m.cfg.CertDir, m.cfg.Domain+".key") }
func (m *certManager) acctPath() string { return filepath.Join(m.cfg.CertDir, "account.key") }

// load 从磁盘读取证书; 校验域名和有效期, 通过才替换内存里的证书。
func (m *certManager) load() error {
	pair, err := tls.LoadX509KeyPair(m.certPath(), m.keyPath())
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if err := leaf.VerifyHostname(m.cfg.Domain); err != nil {
		return fmt.Errorf("磁盘上的证书不是为 %s 签发的", m.cfg.Domain)
	}
	if time.Now().After(leaf.NotAfter) {
		return errors.New("磁盘上的证书已过期")
	}
	pair.Leaf = leaf
	m.mu.Lock()
	m.cert, m.leaf = &pair, leaf
	m.mu.Unlock()
	return nil
}

func (m *certManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, errors.New("证书尚未就绪")
	}
	return m.cert, nil
}

// needsRenewal: 剩余有效期不足总寿命的 1/3 (90 天证书即剩 30 天) 就续。
func needsRenewal(leaf *x509.Certificate, now time.Time) bool {
	if leaf == nil {
		return true
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Sub(now) < life/3
}

func (m *certManager) current() *x509.Certificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leaf
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (m *certManager) accountKey() (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(m.acctPath()); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				return k, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(k)
	if err := writeFileAtomic(m.acctPath(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func (m *certManager) httpClient() (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if m.cfg.CAFile != "" {
		pemBytes, err := os.ReadFile(m.cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("HP_ACME_CA_FILE 里没有有效的证书")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

// issueFunc 向 CA 申请证书, 返回证书链 PEM 与私钥 PEM。测试里可替换。
type issueFunc func(ctx context.Context) (certPEM, keyPEM []byte, err error)

// issueACME 走真正的 ACME DNS-01 流程。
func (m *certManager) issueACME(ctx context.Context) ([]byte, []byte, error) {
	if err := os.MkdirAll(m.cfg.CertDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	key, err := m.accountKey()
	if err != nil {
		return nil, nil, fmt.Errorf("准备账户密钥失败: %w", err)
	}
	hc, err := m.httpClient()
	if err != nil {
		return nil, nil, err
	}
	c := &acmeClient{dirURL: m.cfg.ACMEURL, hc: hc, key: key, logf: log.Printf}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return c.obtain(cctx, acmeOpts{
		Domain: m.cfg.Domain, Email: m.cfg.Email, DNS: m.cfg.DNS,
		Resolver: m.cfg.Resolver, DNSWait: m.cfg.DNSWait,
	})
}

// obtain 申请新证书并落盘、加载。
func (m *certManager) obtain(ctx context.Context) error {
	issue := m.issue
	if issue == nil {
		issue = m.issueACME
	}
	certPEM, keyPEM, err := issue(ctx)
	if err != nil {
		return err
	}
	return m.store(certPEM, keyPEM)
}

// store 先验证拿到的证书能用, 再落盘并加载, 避免把好证书覆盖成坏的。
func (m *certManager) store(certPEM, keyPEM []byte) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("ACME 返回的证书无法使用: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(m.cfg.Domain) != nil {
		return errors.New("ACME 返回的证书与域名不匹配")
	}
	if err := os.MkdirAll(m.cfg.CertDir, 0o700); err != nil {
		return fmt.Errorf("创建证书目录失败: %w", err)
	}
	// 私钥先写, 证书后写: 中途失败时磁盘上仍是旧的一对(或没有)
	if err := writeFileAtomic(m.keyPath(), keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(m.certPath(), certPEM, 0o644); err != nil {
		return err
	}
	return m.load()
}

// run: 启动时尝试加载, 之后每天检查一次。
// 失败按 5m -> 10m -> 20m -> 40m -> 1h 退避 (封顶 1h): 首次配置写错能很快重试看到结果,
// 长期失败也不会逼近 Let's Encrypt 的失败验证限额 (每小时 5 次)。
func (m *certManager) run(ctx context.Context) {
	if err := m.load(); err != nil {
		log.Printf("未找到可用证书 (%v), 将申请新证书", err)
	} else {
		log.Printf("已加载证书 %s, 到期时间 %s", m.cfg.Domain, m.leaf.NotAfter.Format("2006-01-02"))
	}
	minBackoff, maxBackoff := 5*time.Minute, time.Hour
	if m.backoff[0] > 0 {
		minBackoff, maxBackoff = m.backoff[0], m.backoff[1]
	}
	backoff := minBackoff
	for {
		wait := m.checkInterval()
		if needsRenewal(m.current(), time.Now()) {
			log.Printf("开始为 %s 申请/续期证书", m.cfg.Domain)
			if err := m.obtain(ctx); err != nil {
				log.Printf("证书申请失败: %v; %s 后重试", err, backoff)
				wait = backoff
				if backoff *= 2; backoff > maxBackoff {
					backoff = maxBackoff
				}
			} else {
				log.Printf("证书已更新, 到期时间 %s", m.current().NotAfter.Format("2006-01-02"))
				backoff = minBackoff
			}
		}
		if sleepCtx(ctx, wait) != nil {
			return
		}
	}
}

// checkInterval 正常情况下每天检查一次; 测试里可通过 m.interval 缩短。
func (m *certManager) checkInterval() time.Duration {
	if m.interval > 0 {
		return m.interval
	}
	return 24 * time.Hour
}

func (m *certManager) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.getCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}
