// dnsproxy-scheduler 入口：对外只提供 DoH 的 DNS 代理，内部对多家上游做「动态择优 + 并发赛马」。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"

	bootstrapcache "dnsproxy-scheduler/internal/bootstrap"
	"dnsproxy-scheduler/internal/config"
	"dnsproxy-scheduler/internal/ecs"
	"dnsproxy-scheduler/internal/handler"
	"dnsproxy-scheduler/internal/scheduler"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "配置文件路径")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	// bootstrap：用明文 DNS 解析上游主机名（DoH/DoT/DoQ 的 hostname）。
	bootstrap, err := proxy.ParseUpstreamsConfig(cfg.Bootstrap, &upstream.Options{Timeout: cfg.ProbeTimeout})
	if err != nil {
		logger.Error("解析 bootstrap 失败", "err", err)
		os.Exit(1)
	}
	bootstrapUpstreams := bootstrap.Upstreams
	bootstrapResolvers := make(upstream.ParallelResolver, 0, len(bootstrapUpstreams))
	for _, u := range bootstrapUpstreams {
		bootstrapResolvers = append(bootstrapResolvers, &upstream.UpstreamResolver{Upstream: u})
	}

	// 若配置了 bootstrap_cache_ttl > 0，则给引导解析加一层固定 TTL 缓存。
	var bootstrapResolver upstream.Resolver = bootstrapResolvers
	if cfg.BootstrapCacheTTL > 0 {
		bootstrapResolver = bootstrapcache.New(bootstrapResolvers, cfg.BootstrapCacheTTL)
	}

	upstreamOpts := &upstream.Options{
		Bootstrap: bootstrapResolver,
		Timeout:   cfg.ProbeTimeout,
	}

	cert, err := tls.LoadX509KeyPair(cfg.Cert.CertPath, cfg.Cert.KeyPath)
	if err != nil {
		logger.Error("加载 TLS 证书失败", "err", err)
		os.Exit(1)
	}

	ap, err := netip.ParseAddrPort(cfg.ListenAddr())
	if err != nil {
		logger.Error("解析监听地址失败", "addr", cfg.ListenAddr(), "err", err)
		os.Exit(1)
	}

	sched := scheduler.New(cfg, logger, upstreamOpts)

	// ECS 策略：off/override 在 handler 层改请求，pass 依赖库透传。
	ecsPolicy, err := ecs.New(cfg.ECS.Mode, cfg.ECS.Address)
	if err != nil {
		logger.Error("解析 ECS 配置失败", "err", err)
		os.Exit(1)
	}
	// 仅 pass 模式开启库的 ECS 透传与 subnet 缓存分键。
	enableECS := cfg.ECS.Mode == "pass"

	// 占位上游：实际转发走 Handler 注入的 CustomUpstreamConfig，此配置仅在选路未就绪时回退。
	placeholder, err := proxy.ParseUpstreamsConfig([]string{"tls://1.1.1.1:853"}, upstreamOpts)
	if err != nil {
		logger.Error("解析占位上游失败", "err", err)
		os.Exit(1)
	}

	p, err := proxy.New(&proxy.Config{
		Logger:                 logger,
		RequestHandler:         handler.New(sched, logger, ecsPolicy),
		UpstreamConfig:         placeholder,
		UpstreamMode:           proxy.UpstreamModeParallel,
		EnableEDNSClientSubnet: enableECS,
		HTTPConfig: &proxy.HTTPConfig{
			ListenAddresses: []netip.AddrPort{ap},
			ServerHeader:    "dnsproxy-scheduler",
			Routes:          []string{http.MethodGet + " " + cfg.DoHPath, http.MethodPost + " " + cfg.DoHPath},
		},
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	})
	if err != nil {
		logger.Error("创建 proxy 失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 启动调度循环（首轮立即探测）。
	go sched.Start(ctx)

	go func() {
		if err := p.Start(ctx); err != nil {
			logger.Error("proxy 启动失败", "err", err)
			stop()
		}
	}()

	logger.Info("dnsproxy-scheduler 已启动", "listen", cfg.ListenAddr(), "probe_interval", cfg.ProbeInterval)

	<-ctx.Done()
	logger.Info("收到退出信号，正在关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Shutdown(shutdownCtx)
	sched.Stop()
}
