package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("hubproxy-lite", version)
		return
	}
	log.SetFlags(log.LstdFlags)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	memLimit := tuneRuntime()
	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			srv.tokens.sweep()
			srv.mc.sweep()
			srv.lim.sweep()
		}
	}()

	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       0, // git-upload-pack 的请求体可能较大; 由 Stall 逻辑兜底
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", cfg.Listen, err)
	}
	log.Printf("hubproxy-lite %s 已启动: %s | 并发流 %d | 限流 %d/%s | 内存软上限 %s",
		version, cfg.Listen, cfg.MaxStreams, cfg.RateLimit, cfg.RatePeriod, fmtLimit(memLimit))
	if len(cfg.AllowCIDR) == 0 && len(cfg.Allow) == 0 {
		log.Printf("提示: 未设置 HP_ALLOW_CIDR / HP_ALLOW, 当前是对所有能访问该端口的人开放的")
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务异常退出: %v", err)
		}
	case s := <-sig:
		log.Printf("收到 %v, 等待现有连接结束 (最多 20 秒)", s)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}
}

func fmtLimit(n int64) string {
	switch {
	case n < 0:
		return "由 GOMEMLIMIT 指定"
	default:
		return fmt.Sprintf("%dMB", n>>20)
	}
}
