package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// GitHub / Hugging Face 文件加速。
//
// 用法: http://代理地址/https://github.com/owner/repo/releases/download/...
// 与原版的差异:
//   - 主机采用白名单 (精确 + 少量后缀), 不是开放代理, 也不会被用来探测内网
//   - 服务端跟随重定向 (github release -> objects.githubusercontent.com), 目标同样受白名单约束
//   - 支持 git clone (smart HTTP 的 GET info/refs 与 POST git-upload-pack)
//   - .sh/.ps1 脚本里的 GitHub 链接自动改写成走本代理, 上限 10MB

var allowedSuffixes = []string{".githubusercontent.com", ".githubassets.com", ".hf.co", ".huggingface.co"}

func (s *server) hostAllowed(h string) bool {
	h = strings.ToLower(h)
	if s.hosts[h] {
		return true
	}
	for _, suf := range allowedSuffixes {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// 响应里这些类型一律拒绝: 防止把网页 (含钓鱼页) 经由代理域名/IP 暴露出去。
var blockedTypes = map[string]bool{
	"text/html": true, "application/xhtml+xml": true, "text/xml": true, "application/xml": true,
}

const maxScript = 10 << 20

var scriptRe = regexp.MustCompile(`(?:^|[\s'"(=,\[{;|&<>])https?://(?:github\.com|raw\.githubusercontent\.com|raw\.github\.com|gist\.githubusercontent\.com|gist\.github\.com|api\.github\.com)[^\s'")]*`)

func rewriteScript(src []byte, base string) []byte {
	if !strings.Contains(string(src), "github") {
		return src
	}
	return scriptRe.ReplaceAllFunc(src, func(m []byte) []byte {
		str := string(m)
		prefix := ""
		if str[0] != 'h' {
			prefix, str = str[:1], str[1:]
		}
		if strings.HasPrefix(str, "http://") {
			str = "https" + str[4:]
		}
		return []byte(prefix + base + "/" + str)
	})
}

// repoName 从目标 URL 提取 owner/repo, 供黑白名单使用; 取不到返回空串。
func repoName(u *url.URL) string {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch strings.ToLower(u.Hostname()) {
	case "api.github.com":
		if len(segs) >= 3 && segs[0] == "repos" {
			segs = segs[1:]
		} else {
			return ""
		}
	case "github.com", "raw.githubusercontent.com", "raw.github.com", "gist.github.com",
		"gist.githubusercontent.com", "huggingface.co", "hf.co":
	default:
		return ""
	}
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return ""
	}
	return strings.TrimSuffix(segs[0]+"/"+segs[1], ".git")
}

func parseTarget(r *http.Request) (*url.URL, bool) {
	raw := strings.TrimLeft(r.URL.EscapedPath(), "/")
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	// 兼容 "https:/host" (某些客户端/中间层会把 // 合并成 /) 与省略协议的写法
	switch {
	case strings.HasPrefix(raw, "https://"):
	case strings.HasPrefix(raw, "http://"):
		raw = "https://" + raw[len("http://"):]
	case strings.HasPrefix(raw, "https:/"):
		raw = "https://" + raw[len("https:/"):]
	case strings.HasPrefix(raw, "http:/"):
		raw = "https://" + raw[len("http:/"):]
	default:
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Port() != "" {
		return nil, false
	}
	return u, true
}

func (s *server) handleFile(w http.ResponseWriter, r *http.Request, ip string) {
	isGit := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !isGit {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target, ok := parseTarget(r)
	if !ok || !s.hostAllowed(target.Hostname()) {
		http.Error(w, "无效输入: 仅支持 github.com / raw.githubusercontent.com / huggingface.co 等链接", http.StatusForbidden)
		return
	}
	name := repoName(target)
	if len(s.cfg.Allow) > 0 && name == "" {
		http.Error(w, "已启用白名单, 无法识别该链接所属仓库", http.StatusForbidden)
		return
	}
	if name != "" && !s.permitted(name) {
		http.Error(w, "仓库访问被限制", http.StatusForbidden)
		return
	}
	if ok, wait := s.lim.allow(ip); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		http.Error(w, "请求过于频繁", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if !s.acquire(ctx) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "代理繁忙, 请稍后重试", http.StatusServiceUnavailable)
		return
	}
	defer s.release()

	hdr := cleanReqHeaders(r.Header)
	isScript := false
	if r.Method == http.MethodGet {
		lp := strings.ToLower(target.Path)
		isScript = strings.HasSuffix(lp, ".sh") || strings.HasSuffix(lp, ".ps1")
	}
	if isScript {
		hdr.Del("Accept-Encoding") // 要改写文本, 让上游给明文
		hdr.Del("Range")
	}
	var body io.Reader
	var clen int64
	if isGit && r.ContentLength != 0 {
		body, clen = r.Body, r.ContentLength
	}

	resp, err := s.doFile(ctx, r.Method, target.String(), hdr, body, clen)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if r.Method == http.MethodGet {
		ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
		if blockedTypes[ct] {
			http.Error(w, "检测到网页类型, 本服务只加速文件, 请检查链接是否正确", http.StatusForbidden)
			return
		}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n > s.cfg.MaxFileBytes {
			http.Error(w, fmt.Sprintf("文件过大, 上限 %d MB", s.cfg.MaxFileBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
	}

	if isScript && resp.StatusCode == http.StatusOK {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxScript+1))
		if err != nil || len(data) > maxScript {
			http.Error(w, "脚本读取失败或超过 10MB", http.StatusBadGateway)
			return
		}
		out := rewriteScript(data, s.baseURL(r))
		writeRespHeaders(w, resp)
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	}

	writeRespHeaders(w, resp)
	w.Header().Del("Location") // 重定向已在服务端跟随; 残留的 3xx (如 304 之外) 不应把客户端带去别处
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		s.stream(w, io.LimitReader(resp.Body, s.cfg.MaxFileBytes), cancel)
	}
}

// doFile 与 do 类似, 但每一跳重定向都要重新过主机白名单。
func (s *server) doFile(ctx context.Context, method, rawurl string, hdr http.Header, body io.Reader, clen int64) (*http.Response, error) {
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
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("上游返回了无效的重定向")
		}
		if loc.Scheme != "https" || loc.User != nil || loc.Port() != "" || !s.hostAllowed(loc.Hostname()) {
			return nil, fmt.Errorf("拒绝重定向到不在白名单内的地址: %s", loc.Hostname())
		}
		rawurl = loc.String()
	}
	return nil, fmt.Errorf("重定向次数过多")
}
