// dnsproxy-router 入口：对外提供 DoH/DoT/DoQ/明文 DNS 的 DNS 代理，
// 内部对多家上游做「动态择优 + 并发赛马」。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"

	bootstrapcache "dnsproxy-router/internal/bootstrap"
	"dnsproxy-router/internal/config"
	"dnsproxy-router/internal/ecs"
	"dnsproxy-router/internal/handler"
	"dnsproxy-router/internal/scheduler"
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

	baseOpts, err := buildBaseOpts(cfg)
	if err != nil {
		logger.Error("初始化上游选项失败", "err", err)
		os.Exit(1)
	}

	sched := scheduler.New(cfg, logger, baseOpts)

	// ECS 策略：off/override 在 handler 层改请求，pass 依赖库透传。
	ecsPolicy, err := ecs.New(cfg.ECS.Mode, cfg.ECS.Address)
	if err != nil {
		logger.Error("解析 ECS 配置失败", "err", err)
		os.Exit(1)
	}
	// 仅 pass 模式开启库的 ECS 透传与 subnet 缓存分键。
	enableECS := cfg.ECS.Mode == "pass"

	reqHandler := handler.New(sched, logger, ecsPolicy, cfg.DNSSEC)

	// 占位上游：实际转发走 Handler 注入的 CustomUpstreamConfig，此配置仅在选路未就绪时回退。
	placeholder, err := proxy.ParseUpstreamsConfig([]string{"tls://1.1.1.1:853"}, baseOpts)
	if err != nil {
		logger.Error("解析占位上游失败", "err", err)
		os.Exit(1)
	}

	ls := cfg.Listeners

	proxyCfg := &proxy.Config{
		Logger:                 logger,
		RequestHandler:         reqHandler,
		UpstreamConfig:         placeholder,
		UpstreamMode:           proxy.UpstreamModeParallel,
		EnableEDNSClientSubnet: enableECS,
	}

	// 明文 DNS（UDP + TCP 同端口）。
	if ls.PlainDNS.Enabled {
		proxyCfg.UDPListenAddr = []*net.UDPAddr{{Port: ls.PlainDNS.Port}}
		proxyCfg.TCPListenAddr = []*net.TCPAddr{{Port: ls.PlainDNS.Port}}
	}

	// TLS 监听（DoT / DoQ / DoH）共享同一证书。
	if ls.NeedsTLS() {
		cert, err := tls.LoadX509KeyPair(cfg.Cert.CertPath, cfg.Cert.KeyPath)
		if err != nil {
			logger.Error("加载 TLS 证书失败", "err", err)
			os.Exit(1)
		}
		proxyCfg.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		if ls.DoT.Enabled {
			proxyCfg.TLSListenAddr = []*net.TCPAddr{{Port: ls.DoT.Port}}
		}
		if ls.DoQ.Enabled {
			proxyCfg.QUICListenAddr = []*net.UDPAddr{{Port: ls.DoQ.Port}}
		}
		if ls.DoH.Enabled {
			ap, err := netip.ParseAddrPort(fmt.Sprintf("0.0.0.0:%d", ls.DoH.Port))
			if err != nil {
				logger.Error("解析 DoH 监听地址失败", "port", ls.DoH.Port, "err", err)
				os.Exit(1)
			}
			proxyCfg.HTTPConfig = &proxy.HTTPConfig{
				ListenAddresses: []netip.AddrPort{ap},
				ServerHeader:    "dnsproxy-router",
				Routes:          []string{http.MethodGet + " " + ls.DoH.Path, http.MethodPost + " " + ls.DoH.Path},
				// 入站 DoH 是否同时监听 HTTP/3（与 HTTP/2 同 IP:port，QUIC/UDP）。
				HTTP3Enabled: ls.DoH.HTTP3,
			}
		}
	}

	p, err := proxy.New(proxyCfg)
	if err != nil {
		logger.Error("创建 proxy 失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// appCtx 是调度器的运行上下文，刻意与信号 ctx 解耦：进程退出时先经
	// p.Shutdown 排空在途请求，再显式 Stop 当前调度器关闭上游。若直接把信号
	// ctx 传给 Start，SIGTERM 一到调度器立即 closeAllLive，上游会在排空窗口
	// 之前被关闭，破坏优雅关停。
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// SIGHUP 独立监听（不并入 NotifyContext），用于热重载配置。
	sigHup := make(chan os.Signal, 1)
	signal.Notify(sigHup, syscall.SIGHUP)
	defer signal.Stop(sigHup)

	// /healthz 就绪探针：选路未就绪返回 503，就绪返回 200。绑定失败不致命。
	// 用 http.Server 持有句柄，关停时 Close 掉监听，避免 goroutine 泄漏。
	var healthSrv *http.Server
	if cfg.HealthHTTP != "" {
		ln, err := net.Listen("tcp", cfg.HealthHTTP)
		if err != nil {
			logger.Error("健康检查监听失败", "addr", cfg.HealthHTTP, "err", err)
		} else {
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				if reqHandler.Ready() {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			healthSrv = &http.Server{Handler: mux}
			go func() {
				if err := healthSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Error("健康检查服务异常", "err", err)
				}
			}()
		}
	}

	// 启动调度循环（首轮立即探测；per-provider 的延迟探测器由 scheduler 内部管理）。
	go sched.Start(appCtx)

	// SIGHUP 热重载循环：重读配置 → 重建 scheduler → 原子切换 → 旧调度器优雅退役。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigHup:
				newCfg, err := config.LoadConfig(configPath)
				if err != nil {
					logger.Error("热重载失败，沿用旧配置", "err", err)
					continue
				}
				newBaseOpts, err := buildBaseOpts(newCfg)
				if err != nil {
					logger.Error("热重载失败（上游选项），沿用旧配置", "err", err)
					continue
				}
				newSched := scheduler.New(newCfg, logger, newBaseOpts)
				go newSched.Start(appCtx)
				// 先等新调度器完成首轮选路再切换：若直接 swap，首轮探测窗口内
				// 新调度器 CurrentConfig 仍为 nil，所有请求会回退占位上游并丢缓存。
				select {
				case <-newSched.FirstDone():
					if newSched.CurrentConfig() == nil {
						// 新配置所有上游均不可用：放弃切换，沿用旧配置。
						logger.Error("热重载失败（新配置所有上游均不可用），沿用旧配置")
						newSched.Retire(0)
						continue
					}
				case <-ctx.Done():
					newSched.Retire(0)
					return
				}
				old := reqHandler.Swap(newSched)
				if old != nil {
					old.Retire(2 * time.Duration(newCfg.ProbeTimeout))
				}
				logger.Info("配置已热重载", "probe_interval", newCfg.ProbeInterval)
			}
		}
	}()

	go func() {
		if err := p.Start(ctx); err != nil {
			logger.Error("proxy 启动失败", "err", err)
			stop()
		}
	}()

	logger.Info("dnsproxy-router 已启动",
		"doh", ls.DoH.Enabled, "dot", ls.DoT.Enabled, "doq", ls.DoQ.Enabled,
		"plain_dns", ls.PlainDNS.Enabled, "probe_interval", cfg.ProbeInterval)

	<-ctx.Done()
	logger.Info("收到退出信号，正在关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Shutdown(shutdownCtx)
	// 关闭 /healthz 监听。
	if healthSrv != nil {
		_ = healthSrv.Close()
	}
	// 排空完成后，停止当前生效的调度器（可能已因热重载 swap 成新实例）。
	// 先 cancel appCtx 使调度循环与 prober 退出，再显式关闭其存活上游。
	appCancel()
	if cur := reqHandler.Current(); cur != nil {
		cur.Stop()
	}
}

// buildBaseOpts 构造共享的基础上游选项：引导解析链（明文 DNS → 缓存 → hosts）
// 与 DoH HTTP/3 优先级。启动与 SIGHUP 热重载共用。
func buildBaseOpts(cfg config.Config) (*upstream.Options, error) {
	// bootstrap：用明文 DNS 解析上游主机名（DoH/DoT/DoQ 的 hostname）。
	bootstrap, err := proxy.ParseUpstreamsConfig(cfg.Bootstrap, &upstream.Options{Timeout: time.Duration(cfg.ProbeTimeout)})
	if err != nil {
		return nil, fmt.Errorf("解析 bootstrap: %w", err)
	}
	bootstrapUpstreams := bootstrap.Upstreams
	bootstrapResolvers := make(upstream.ParallelResolver, 0, len(bootstrapUpstreams))
	for _, u := range bootstrapUpstreams {
		bootstrapResolvers = append(bootstrapResolvers, &upstream.UpstreamResolver{Upstream: u})
	}

	// 若配置了 bootstrap_cache_ttl > 0，则给引导解析加一层固定 TTL 缓存。
	var bootstrapResolver upstream.Resolver = bootstrapResolvers
	if cfg.BootstrapCacheTTL > 0 {
		bootstrapResolver = bootstrapcache.New(bootstrapResolvers, time.Duration(cfg.BootstrapCacheTTL))
	}

	// 域名 → IP 静态映射：命中即直接用给定 IP，未命中回退引导 DNS。
	if len(cfg.Hosts) > 0 {
		hosts := make(map[string][]netip.Addr, len(cfg.Hosts))
		for name, addrs := range cfg.Hosts {
			ips := make([]netip.Addr, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, netip.MustParseAddr(a))
			}
			hosts[name] = ips
		}
		bootstrapResolver = bootstrapcache.NewHostsResolver(hosts, bootstrapResolver)
	}

	// 基础上游选项：Bootstrap 为共享引导解析链（明文 DNS → 缓存 → hosts），
	// PreferIPv6 固定 false——per-provider 的优先级由 scheduler 按
	// provider_ip_priority 各自 Clone 覆盖，这里只提供兜底。
	return &upstream.Options{
		Bootstrap:  bootstrapResolver,
		Timeout:    time.Duration(cfg.ProbeTimeout),
		PreferIPv6: false,
		// DoH 上游优先尝试 HTTP/3：首次建连时库会并发探测 QUIC 与 TLS，
		// QUIC 更快且可用则走 h3，否则自动降级回 HTTP/2。探测后 client 被
		// 缓存复用，连接跨请求保持热。该字段仅对 DoH（https://）生效，
		// DoT/DoQ/Plain 直接忽略。
		HTTPVersions: []upstream.HTTPVersion{
			upstream.HTTPVersion3,
			upstream.HTTPVersion2,
			upstream.HTTPVersion11,
		},
	}, nil
}
