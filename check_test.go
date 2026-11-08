package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDNS struct {
	presentErr, cleanupErr error
	presented, cleaned     []string
}

func (f *fakeDNS) Present(_ context.Context, fqdn, v string) error {
	f.presented = append(f.presented, fqdn+"|"+v)
	return f.presentErr
}
func (f *fakeDNS) Cleanup(_ context.Context, fqdn, v string) error {
	f.cleaned = append(f.cleaned, fqdn+"|"+v)
	return f.cleanupErr
}

func TestCheckDNSOK(t *testing.T) {
	f := &fakeDNS{}
	var out bytes.Buffer
	if err := checkDNS(context.Background(), f, "hub.example.com", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.presented) != 1 || len(f.cleaned) != 1 || f.presented[0] != f.cleaned[0] {
		t.Fatalf("应写入一条并清理同一条: %v %v", f.presented, f.cleaned)
	}
	if !strings.HasPrefix(f.presented[0], "_hubproxy-check.hub.example.com|") {
		t.Fatalf("测试记录名不对 (不能用 _acme-challenge): %s", f.presented[0])
	}
	if strings.Contains(f.presented[0], "_acme-challenge") {
		t.Fatal("不应碰 ACME 验证记录")
	}
}

func TestCheckDNSWriteFails(t *testing.T) {
	f := &fakeDNS{presentErr: errors.New("403 forbidden")}
	err := checkDNS(context.Background(), f, "hub.example.com", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "写入失败") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("应报写入失败并带上原因: %v", err)
	}
	if len(f.cleaned) != 0 {
		t.Fatal("写入失败时不应调用清理")
	}
}

func TestCheckDNSCleanupFailsIsReported(t *testing.T) {
	f := &fakeDNS{cleanupErr: errors.New("boom")}
	err := checkDNS(context.Background(), f, "hub.example.com", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "手动删除") {
		t.Fatalf("清理失败必须报告并提示手动删除: %v", err)
	}
}
