package main

// check-dns 子命令: 安装前验证 DNS 配置真的可用 (Token 权限、域名是否在该账户下、hook 能否执行)。
// 做法是真实写入再删除一条测试 TXT 记录, 这样连"写权限"也能测到; 只读检查测不出 DNS:Edit 缺失。
// 用的是 _hubproxy-check.<域名>, 不会碰 ACME 的 _acme-challenge 记录。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// checkDNS 写入再清理一条测试记录。清理失败也算失败: 留下垃圾记录要让用户知道。
func checkDNS(ctx context.Context, p dnsProvider, domain string, out io.Writer) error {
	name := "_hubproxy-check." + domain
	val := fmt.Sprintf("hubproxy-lite-check-%d", time.Now().Unix())
	fmt.Fprintf(out, "写入测试记录 %s ...\n", name)
	if err := p.Present(ctx, name, val); err != nil {
		return fmt.Errorf("写入失败: %w", err)
	}
	fmt.Fprintln(out, "写入成功, 清理中 ...")
	if err := p.Cleanup(ctx, name, val); err != nil {
		return fmt.Errorf("测试记录已写入但清理失败 (请到 DNS 控制台手动删除 %s): %w", name, err)
	}
	fmt.Fprintln(out, "DNS 配置可用")
	return nil
}

// runCheckDNS 读取与服务相同的环境变量, 供 install.sh 在安装前调用。
func runCheckDNS(out io.Writer) error {
	t, err := loadTLSConfig()
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("未设置 HP_DOMAIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return checkDNS(ctx, t.DNS, strings.ToLower(t.Domain), out)
}
