package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Docker Registry v2 代理。
//
// 与原版最大的区别: 不再用 go-containerregistry 重新拉取再转发, 而是直接透传 HTTP,
// 由服务端自己向上游换取匿名 pull token。客户端永远不需要认证, 所以:
//   - /v2/ 恒返回 200, 不需要改写 Www-Authenticate 的 realm (无域名/HTTP 场景下最容易出错的一环)
//   - blob 的 307 跳转 (指向 CDN) 由服务端跟随, 客户端只和代理通信
//   - 只能拉取公开镜像, 不转发任何客户端凭据

type regTarget struct {
	host string // 上游主机
	name string // 上游仓库名
	kind string // manifests | blobs | tags
	ref  string // manifests: tag/digest; blobs: digest
	perm string // 用于黑白名单匹配的名称
}

func validName(s string) bool {
	if s == "" || len(s) > 255 || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		s[0] == '/' || s[len(s)-1] == '/' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' || c == '/') {
			return false
		}
	}
	return true
}

func validRef(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

func (s *server) parseRegistry(r *http.Request) (regTarget, bool) {
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	host := s.hubHost
	prefixed := ""
	if seg, tail, ok := strings.Cut(rest, "/"); ok {
		if up, found := s.regs[strings.ToLower(seg)]; found {
			host, rest, prefixed = up, tail, strings.ToLower(seg)
		}
	}
	if prefixed == "" { // 兼容 containerd / docker 镜像加速发来的 ?ns=xxx
		if ns := strings.ToLower(r.URL.Query().Get("ns")); ns != "" {
			if up, found := s.regs[ns]; found {
				host, prefixed = up, ns
			}
		}
	}

	var t regTarget
	switch {
	case strings.Contains(rest, "/manifests/"):
		i := strings.Index(rest, "/manifests/")
		t.name, t.kind, t.ref = rest[:i], "manifests", rest[i+len("/manifests/"):]
	case strings.Contains(rest, "/blobs/"):
		i := strings.Index(rest, "/blobs/")
		t.name, t.kind, t.ref = rest[:i], "blobs", rest[i+len("/blobs/"):]
	case strings.HasSuffix(rest, "/tags/list"):
		t.name, t.kind, t.ref = strings.TrimSuffix(rest, "/tags/list"), "tags", ""
	default:
		return t, false
	}
	t.host = host
	if host == s.hubHost && !strings.Contains(t.name, "/") {
		t.name = "library/" + t.name
	}
	if !validName(t.name) || (t.kind != "tags" && !validRef(t.ref)) {
		return t, false
	}
	t.perm = t.name
	if prefixed != "" && host != s.hubHost {
		t.perm = prefixed + "/" + t.name
	}
	return t, true
}

func (s *server) handleRegistry(w http.ResponseWriter, r *http.Request, ip string) {
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		io.WriteString(w, "{}")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "只读代理, 不支持推送", http.StatusMethodNotAllowed)
		return
	}
	t, ok := s.parseRegistry(r)
	if !ok {
		http.Error(w, "unsupported registry path", http.StatusNotFound)
		return
	}
	if !s.permitted(t.perm) {
		http.Error(w, "镜像访问被限制", http.StatusForbidden)
		return
	}
	if ok, wait := s.lim.allow(ip); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		http.Error(w, "请求过于频繁", http.StatusTooManyRequests)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Stall))

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	upath := "/v2/" + t.name + "/" + t.kind
	if t.kind == "tags" {
		upath += "/list"
	} else {
		upath += "/" + t.ref
	}
	rawq := ""
	if t.kind == "tags" {
		q := r.URL.Query()
		q.Del("ns")
		rawq = q.Encode()
	}

	cacheKey := ""
	if t.kind == "manifests" && r.Method == http.MethodGet && s.cfg.ManifestCache {
		cacheKey = t.host + "|" + t.name + "|" + t.ref + "|" + r.Header.Get("Accept")
		if it := s.mc.get(cacheKey); it != nil {
			h := w.Header()
			for k, v := range it.hdr {
				h[k] = v
			}
			w.WriteHeader(http.StatusOK)
			w.Write(it.body)
			return
		}
	}

	if t.kind == "blobs" { // 只有大流量的 blob 受并发上限约束
		if !s.acquire(ctx) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "代理繁忙, 请稍后重试", http.StatusServiceUnavailable)
			return
		}
		defer s.release()
	}

	resp, err := s.fetchRegistry(ctx, r, t, upath, rawq)
	if err != nil {
		log.Printf("registry %s/%s: %v", t.host, t.name, err)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if cacheKey != "" && resp.StatusCode == http.StatusOK && resp.ContentLength <= mcMaxItem {
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, mcMaxItem+1))
		if rerr != nil {
			http.Error(w, "upstream read error", http.StatusBadGateway)
			return
		}
		if len(buf) <= mcMaxItem {
			hdr := http.Header{}
			for _, k := range []string{"Content-Type", "Docker-Content-Digest", "Etag"} {
				if v := resp.Header.Get(k); v != "" {
					hdr.Set(k, v)
				}
			}
			hdr.Set("Content-Length", fmt.Sprint(len(buf)))
			s.mc.set(cacheKey, &mcItem{body: buf, hdr: hdr, exp: time.Now().Add(manifestTTL(t.ref))})
			h := w.Header()
			for k, v := range hdr {
				h[k] = v
			}
			w.WriteHeader(http.StatusOK)
			w.Write(buf)
			return
		}
		body = io.MultiReader(bytes.NewReader(buf), resp.Body) // 太大: 不缓存, 原样流式输出
	}

	writeRespHeaders(w, resp)
	w.Header().Del("Www-Authenticate") // 上游的 realm 指向它自己, 对客户端没有意义
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		s.stream(w, body, cancel)
	}
}

func (s *server) fetchRegistry(ctx context.Context, r *http.Request, t regTarget, upath, rawq string) (*http.Response, error) {
	hdr := http.Header{}
	for _, k := range []string{"Accept", "Range", "If-None-Match"} {
		if v := r.Header.Values(k); len(v) > 0 {
			hdr[k] = append([]string(nil), v...)
		}
	}
	hdr.Set("User-Agent", "hubproxy-lite/"+version)
	rawurl := s.scheme + "://" + t.host + upath
	if rawq != "" {
		rawurl += "?" + rawq
	}
	force := false
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := s.getToken(ctx, t.host, t.name, force)
		if err != nil {
			return nil, err
		}
		h := hdr.Clone()
		if tok != "" {
			h.Set("Authorization", "Bearer "+tok)
		}
		resp, err := s.do(ctx, r.Method, rawurl, h, nil, 0)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 { // token 过期: 强制换新再试一次
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			force = true
			continue
		}
		return resp, nil
	}
	return nil, errors.New("认证失败")
}

// ---------------------------------------------------------------- 上游匿名 token

func headerParam(h, key string) string {
	lh := strings.ToLower(h)
	i := strings.Index(lh, key+`="`)
	if i < 0 {
		return ""
	}
	v := h[i+len(key)+2:]
	if j := strings.IndexByte(v, '"'); j >= 0 {
		return v[:j]
	}
	return ""
}

func (s *server) discover(ctx context.Context, host string) (authInfo, error) {
	s.tokens.mu.Lock()
	ai, ok := s.tokens.auth[host]
	s.tokens.mu.Unlock()
	if ok {
		return ai, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.scheme+"://"+host+"/v2/", nil)
	if err != nil {
		return ai, err
	}
	req.Header.Set("User-Agent", "hubproxy-lite/"+version)
	resp, err := s.client.Do(req)
	if err != nil {
		return ai, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
		ai = authInfo{none: true}
	case http.StatusUnauthorized:
		wa := resp.Header.Get("Www-Authenticate")
		if strings.HasPrefix(strings.ToLower(wa), "bearer") && headerParam(wa, "realm") != "" {
			ai = authInfo{realm: headerParam(wa, "realm"), service: headerParam(wa, "service")}
		} else {
			ai = authInfo{none: true}
		}
	default:
		return ai, fmt.Errorf("探测 %s 认证方式失败: HTTP %d", host, resp.StatusCode)
	}
	s.tokens.mu.Lock()
	s.tokens.auth[host] = ai
	s.tokens.mu.Unlock()
	return ai, nil
}

func (s *server) getToken(ctx context.Context, host, name string, force bool) (string, error) {
	ai, err := s.discover(ctx, host)
	if err != nil {
		return "", err
	}
	if ai.none {
		return "", nil
	}
	key := host + "|" + name
	if !force {
		s.tokens.mu.Lock()
		e, ok := s.tokens.tok[key]
		s.tokens.mu.Unlock()
		if ok && time.Now().Before(e.exp) {
			return e.tok, nil
		}
	}
	u, err := url.Parse(ai.realm)
	if err != nil || u.Scheme != s.scheme {
		return "", fmt.Errorf("上游认证地址不可用: %q", ai.realm)
	}
	q := u.Query()
	if ai.service != "" {
		q.Set("service", ai.service)
	}
	q.Set("scope", "repository:"+name+":pull")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hubproxy-lite/"+version)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("获取 %s token 失败: HTTP %d", host, resp.StatusCode)
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tr); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %v", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", errors.New("上游未返回 token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	ttl = min(ttl-20*time.Second, 5*time.Minute)
	if ttl < 10*time.Second {
		ttl = 10 * time.Second
	}
	s.tokens.mu.Lock()
	s.tokens.tok[key] = tokEntry{tok: tok, exp: time.Now().Add(ttl)}
	s.tokens.mu.Unlock()
	return tok, nil
}
