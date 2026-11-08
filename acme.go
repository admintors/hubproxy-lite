package main

// 极简 ACME (RFC 8555) 客户端: 只做 dns-01 + 单个域名 + ECDSA P-256, 仅用标准库。
// 为什么选 dns-01: 它不要求开放 80/443 端口, 只要能改解析记录就行。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	acmeProdURL    = "https://acme-v02.api.letsencrypt.org/directory"
	acmeStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

type acmeError struct {
	Status int
	Type   string
	Detail string
}

func (e *acmeError) Error() string {
	return fmt.Sprintf("ACME 服务器返回 %d (%s): %s", e.Status, e.Type, e.Detail)
}

// dnsProvider 负责写入/清理 _acme-challenge 的 TXT 记录。
type dnsProvider interface {
	Present(ctx context.Context, fqdn, value string) error
	Cleanup(ctx context.Context, fqdn, value string) error
}

type acmeClient struct {
	dirURL string
	hc     *http.Client
	key    *ecdsa.PrivateKey
	kid    string
	nonce  string
	dir    struct {
		NewNonce   string `json:"newNonce"`
		NewAccount string `json:"newAccount"`
		NewOrder   string `json:"newOrder"`
	}
	logf func(format string, args ...any)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwk 的字段顺序必须是 crv, kty, x, y (字典序), 否则指纹不对。
type jwk struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (c *acmeClient) jwk() jwk {
	p := c.key.PublicKey
	return jwk{
		Crv: "P-256", Kty: "EC",
		X: b64(p.X.FillBytes(make([]byte, 32))),
		Y: b64(p.Y.FillBytes(make([]byte, 32))),
	}
}

func (c *acmeClient) thumbprint() string {
	j, _ := json.Marshal(c.jwk())
	h := sha256.Sum256(j)
	return b64(h[:])
}

// sign 生成扁平 JWS。payload 为 nil 表示 POST-as-GET (空 payload)。
func (c *acmeClient) sign(url, nonce string, payload []byte) ([]byte, error) {
	type protected struct {
		Alg   string `json:"alg"`
		Nonce string `json:"nonce"`
		URL   string `json:"url"`
		JWK   *jwk   `json:"jwk,omitempty"`
		KID   string `json:"kid,omitempty"`
	}
	p := protected{Alg: "ES256", Nonce: nonce, URL: url}
	if c.kid == "" {
		j := c.jwk()
		p.JWK = &j
	} else {
		p.KID = c.kid
	}
	pj, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	pb := b64(pj)
	plb := ""
	if payload != nil {
		plb = b64(payload)
	}
	digest := sha256.Sum256([]byte(pb + "." + plb))
	der, err := ecdsa.SignASN1(rand.Reader, c.key, digest[:])
	if err != nil {
		return nil, err
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &rs); err != nil {
		return nil, err
	}
	sig := append(rs.R.FillBytes(make([]byte, 32)), rs.S.FillBytes(make([]byte, 32))...)
	return json.Marshal(map[string]string{"protected": pb, "payload": plb, "signature": b64(sig)})
}

func (c *acmeClient) loadDirectory(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.dirURL, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 ACME 服务器 %s: %w", c.dirURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("读取 ACME 目录失败: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c.dir); err != nil {
		return fmt.Errorf("解析 ACME 目录失败: %w", err)
	}
	if c.dir.NewNonce == "" || c.dir.NewAccount == "" || c.dir.NewOrder == "" {
		return errors.New("ACME 目录缺少必要字段")
	}
	return nil
}

func (c *acmeClient) fetchNonce(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, c.dir.NewNonce, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.nonce = resp.Header.Get("Replay-Nonce")
	if c.nonce == "" {
		return errors.New("ACME 服务器没有返回 nonce")
	}
	return nil
}

// post 发送签名请求, 遇到 badNonce 自动重试。payload 为 nil 即 POST-as-GET。
func (c *acmeClient) post(ctx context.Context, url string, payload any) (http.Header, []byte, error) {
	var pl []byte
	if payload != nil {
		var err error
		if pl, err = json.Marshal(payload); err != nil {
			return nil, nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		if c.nonce == "" {
			if err := c.fetchNonce(ctx); err != nil {
				return nil, nil, err
			}
		}
		body, err := c.sign(url, c.nonce, pl)
		c.nonce = ""
		if err != nil {
			return nil, nil, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/jose+json")
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if n := resp.Header.Get("Replay-Nonce"); n != "" {
			c.nonce = n
		}
		if resp.StatusCode >= 400 {
			ae := &acmeError{Status: resp.StatusCode}
			var pj struct{ Type, Detail string }
			if json.Unmarshal(data, &pj) == nil {
				ae.Type, ae.Detail = pj.Type, pj.Detail
			}
			if strings.HasSuffix(ae.Type, ":badNonce") && attempt < 2 {
				continue
			}
			return nil, nil, ae
		}
		return resp.Header, data, nil
	}
}

func (c *acmeClient) register(ctx context.Context, email string) error {
	if err := c.loadDirectory(ctx); err != nil {
		return err
	}
	req := map[string]any{"termsOfServiceAgreed": true}
	if email != "" {
		req["contact"] = []string{"mailto:" + email}
	}
	h, _, err := c.post(ctx, c.dir.NewAccount, req)
	if err != nil {
		return fmt.Errorf("注册 ACME 账户失败: %w", err)
	}
	c.kid = h.Get("Location")
	if c.kid == "" {
		return errors.New("ACME 服务器没有返回账户地址")
	}
	return nil
}

type acmeChallenge struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Token  string `json:"token"`
	Status string `json:"status"`
	Error  *struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	} `json:"error"`
}

type acmeAuthz struct {
	Status     string          `json:"status"`
	Challenges []acmeChallenge `json:"challenges"`
	Identifier struct {
		Value string `json:"value"`
	} `json:"identifier"`
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfter(h http.Header, def time.Duration) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 30 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// waitTXT 通过指定的公共解析器轮询, 直到 TXT 记录可见。超时返回 false (调用方仍可继续尝试)。
func waitTXT(ctx context.Context, resolver, fqdn, want string, max time.Duration) bool {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, "udp", resolver)
	}}
	deadline := time.Now().Add(max)
	for {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		txts, err := r.LookupTXT(cctx, fqdn)
		cancel()
		if err == nil {
			for _, t := range txts {
				if t == want {
					return true
				}
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		if sleepCtx(ctx, 3*time.Second) != nil {
			return false
		}
	}
}

type acmeOpts struct {
	Domain   string
	Email    string
	DNS      dnsProvider
	Resolver string        // 用来确认 TXT 已生效的解析器 host:port
	DNSWait  time.Duration // 最长等待解析生效的时间
}

// obtain 走完整个流程, 返回证书链 PEM 与私钥 PEM。
func (c *acmeClient) obtain(ctx context.Context, o acmeOpts) (certPEM, keyPEM []byte, err error) {
	if err := c.register(ctx, o.Email); err != nil {
		return nil, nil, err
	}
	c.logf("ACME 账户就绪, 开始为 %s 下单", o.Domain)

	orderURL, body, err := c.orderCreate(ctx, o.Domain)
	if err != nil {
		return nil, nil, err
	}
	var order struct {
		Status         string   `json:"status"`
		Authorizations []string `json:"authorizations"`
		Finalize       string   `json:"finalize"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		return nil, nil, fmt.Errorf("解析订单失败: %w", err)
	}
	if orderURL == "" || order.Finalize == "" || len(order.Authorizations) == 0 {
		return nil, nil, errors.New("ACME 订单信息不完整")
	}

	for _, au := range order.Authorizations {
		if err := c.solve(ctx, au, o); err != nil {
			return nil, nil, err
		}
	}

	// 生成证书私钥和 CSR
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{o.Domain}}, certKey)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := c.post(ctx, order.Finalize, map[string]string{"csr": b64(csr)}); err != nil {
		return nil, nil, fmt.Errorf("提交 CSR 失败: %w", err)
	}

	// 轮询订单直到 valid
	var certURL string
	for i := 0; ; i++ {
		h, b, err := c.post(ctx, orderURL, nil)
		if err != nil {
			return nil, nil, err
		}
		var st struct {
			Status      string `json:"status"`
			Certificate string `json:"certificate"`
		}
		_ = json.Unmarshal(b, &st)
		if st.Status == "valid" && st.Certificate != "" {
			certURL = st.Certificate
			break
		}
		if st.Status == "invalid" {
			return nil, nil, errors.New("订单被 ACME 服务器标记为 invalid")
		}
		if i > 60 {
			return nil, nil, errors.New("等待签发超时")
		}
		if err := sleepCtx(ctx, retryAfter(h, 2*time.Second)); err != nil {
			return nil, nil, err
		}
	}
	_, certPEM, err = c.post(ctx, certURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("下载证书失败: %w", err)
	}
	kb, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM, nil
}

// orderCreate 返回订单地址 (Location 头) 和订单内容。
func (c *acmeClient) orderCreate(ctx context.Context, domain string) (string, []byte, error) {
	req := map[string]any{"identifiers": []map[string]string{{"type": "dns", "value": domain}}}
	h, b, err := c.post(ctx, c.dir.NewOrder, req)
	if err != nil {
		return "", nil, fmt.Errorf("创建订单失败: %w", err)
	}
	return h.Get("Location"), b, nil
}

// solve 处理一个 authorization: 写 TXT -> 等生效 -> 通知 -> 轮询 -> 清理。
func (c *acmeClient) solve(ctx context.Context, authzURL string, o acmeOpts) error {
	_, b, err := c.post(ctx, authzURL, nil)
	if err != nil {
		return fmt.Errorf("读取授权失败: %w", err)
	}
	var az acmeAuthz
	if err := json.Unmarshal(b, &az); err != nil {
		return err
	}
	if az.Status == "valid" {
		c.logf("%s 的授权已有效, 跳过验证", o.Domain)
		return nil
	}
	var ch *acmeChallenge
	for i := range az.Challenges {
		if az.Challenges[i].Type == "dns-01" {
			ch = &az.Challenges[i]
		}
	}
	if ch == nil {
		return errors.New("ACME 服务器没有提供 dns-01 验证方式")
	}
	sum := sha256.Sum256([]byte(ch.Token + "." + c.thumbprint()))
	value := b64(sum[:])
	fqdn := "_acme-challenge." + o.Domain

	c.logf("写入 TXT 记录 %s", fqdn)
	if err := o.DNS.Present(ctx, fqdn, value); err != nil {
		return fmt.Errorf("写入 DNS 记录失败: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := o.DNS.Cleanup(cctx, fqdn, value); err != nil {
			c.logf("警告: 清理 TXT 记录失败 (可手动删除 %s): %v", fqdn, err)
		}
	}()

	if o.Resolver != "" && o.DNSWait > 0 {
		c.logf("等待解析生效 (最多 %s)", o.DNSWait.Round(time.Second))
		if !waitTXT(ctx, o.Resolver, fqdn, value, o.DNSWait) {
			c.logf("警告: 在 %s 上没有确认到 TXT 记录, 仍然继续请求验证", o.Resolver)
		}
	}
	if _, _, err := c.post(ctx, ch.URL, struct{}{}); err != nil {
		return fmt.Errorf("通知验证失败: %w", err)
	}
	for i := 0; ; i++ {
		h, b, err := c.post(ctx, authzURL, nil)
		if err != nil {
			return err
		}
		var st acmeAuthz
		_ = json.Unmarshal(b, &st)
		switch st.Status {
		case "valid":
			c.logf("域名验证通过")
			return nil
		case "invalid":
			msg := "未知原因"
			for _, x := range st.Challenges {
				if x.Error != nil {
					msg = x.Error.Detail
				}
			}
			return fmt.Errorf("域名验证失败: %s", msg)
		}
		if i > 60 {
			return errors.New("等待域名验证超时")
		}
		if err := sleepCtx(ctx, retryAfter(h, 2*time.Second)); err != nil {
			return err
		}
	}
}
