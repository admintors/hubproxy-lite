package main

// dns-01 用到的 DNS 提供商: cloudflare (HTTP API) 和 exec (调用外部脚本, 兼容任意解析商)。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------ Cloudflare

type cloudflareDNS struct {
	token  string
	zoneID string // 可选; 不填则按域名自动查找
	base   string // 默认 https://api.cloudflare.com/client/v4, 测试时可替换
	hc     *http.Client

	mu  sync.Mutex
	ids map[string]string // fqdn|value -> record id
	zid map[string]string // 域名 -> zone id 缓存
}

func newCloudflareDNS(token, zoneID string) *cloudflareDNS {
	return &cloudflareDNS{
		token: token, zoneID: zoneID,
		base: "https://api.cloudflare.com/client/v4",
		hc:   &http.Client{Timeout: 20 * time.Second},
		ids:  map[string]string{}, zid: map[string]string{},
	}
}

type cfResp struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *cloudflareDNS) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Cloudflare 失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var r cfResp
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("Cloudflare 返回了无法解析的内容 (HTTP %d)", resp.StatusCode)
	}
	if !r.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(r.Errors) > 0 {
			msg = fmt.Sprintf("%s (code %d)", r.Errors[0].Message, r.Errors[0].Code)
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			msg += "; 请检查 Token 权限: 需要 Zone:Zone:Read 和 Zone:DNS:Edit"
		}
		return nil, errors.New("Cloudflare: " + msg)
	}
	return r.Result, nil
}

// findZone 从最长的域名后缀开始逐级往上找, 找到第一个在该账户下存在的 zone。
func (c *cloudflareDNS) findZone(ctx context.Context, fqdn string) (string, error) {
	if c.zoneID != "" {
		return c.zoneID, nil
	}
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i+1 < len(labels); i++ { // 至少保留两段, 如 example.com
		name := strings.Join(labels[i:], ".")
		c.mu.Lock()
		id, ok := c.zid[name]
		c.mu.Unlock()
		if ok {
			return id, nil
		}
		res, err := c.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil)
		if err != nil {
			return "", err
		}
		var zs []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(res, &zs) == nil && len(zs) > 0 {
			c.mu.Lock()
			c.zid[name] = zs[0].ID
			c.mu.Unlock()
			return zs[0].ID, nil
		}
	}
	return "", fmt.Errorf("在 Cloudflare 账户下找不到 %s 所属的域名(zone), 请确认域名已托管在 Cloudflare 且 Token 有权限", fqdn)
}

func (c *cloudflareDNS) Present(ctx context.Context, fqdn, value string) error {
	zid, err := c.findZone(ctx, fqdn)
	if err != nil {
		return err
	}
	res, err := c.call(ctx, http.MethodPost, "/zones/"+zid+"/dns_records", map[string]any{
		"type": "TXT", "name": fqdn, "content": value, "ttl": 60,
	})
	if err != nil {
		return err
	}
	var rec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res, &rec); err != nil || rec.ID == "" {
		return errors.New("Cloudflare 没有返回记录 ID")
	}
	c.mu.Lock()
	c.ids[fqdn+"|"+value] = zid + "/" + rec.ID
	c.mu.Unlock()
	return nil
}

func (c *cloudflareDNS) Cleanup(ctx context.Context, fqdn, value string) error {
	c.mu.Lock()
	key := c.ids[fqdn+"|"+value]
	delete(c.ids, fqdn+"|"+value)
	c.mu.Unlock()
	if key == "" {
		return nil
	}
	zid, rid, _ := strings.Cut(key, "/")
	_, err := c.call(ctx, http.MethodDelete, "/zones/"+zid+"/dns_records/"+rid, nil)
	return err
}

// ------------------------------------------------------------ exec 钩子

// execDNS 调用: <script> present|cleanup <fqdn> <value>
// 脚本退出码非 0 视为失败; 不经过 shell, 参数不会被注入。
type execDNS struct{ script string }

func (e *execDNS) run(ctx context.Context, action, fqdn, value string) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.script, action, fqdn, value)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(out.String())
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("DNS 脚本 %s %s 失败: %v: %s", e.script, action, err, tail)
	}
	return nil
}

func (e *execDNS) Present(ctx context.Context, fqdn, value string) error {
	return e.run(ctx, "present", fqdn, value)
}
func (e *execDNS) Cleanup(ctx context.Context, fqdn, value string) error {
	return e.run(ctx, "cleanup", fqdn, value)
}

// ------------------------------------------------------------ 构造

func buildDNSProvider(provider, cfToken, cfZone, hook string) (dnsProvider, error) {
	switch provider {
	case "cloudflare":
		if cfToken == "" {
			return nil, errors.New("使用 cloudflare 需要设置 HP_CF_TOKEN")
		}
		return newCloudflareDNS(cfToken, cfZone), nil
	case "exec":
		if hook == "" {
			return nil, errors.New("使用 exec 需要设置 HP_DNS_HOOK (脚本路径)")
		}
		st, err := os.Stat(hook)
		if err != nil {
			return nil, fmt.Errorf("HP_DNS_HOOK 指向的脚本不存在: %s", hook)
		}
		if st.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("HP_DNS_HOOK 脚本没有执行权限: %s", hook)
		}
		return &execDNS{script: hook}, nil
	case "":
		return nil, errors.New("启用 HTTPS 需要设置 HP_DNS_PROVIDER (cloudflare 或 exec)")
	}
	return nil, fmt.Errorf("不支持的 HP_DNS_PROVIDER: %q (可选 cloudflare / exec)", provider)
}
