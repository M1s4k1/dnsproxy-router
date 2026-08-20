// Package scheduler 周期性探测各家各模式的延迟，并维护当前的最优选路。
package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"dnsproxy-scheduler/internal/cache"
	"dnsproxy-scheduler/internal/config"
)

// modeOrder 定义探测时模式的稳定遍历顺序，避免 map 无序导致日志抖动。
var modeOrder = []string{"DNS-over-HTTPS", "DNS-over-TLS", "DNS-over-QUIC", "Plain DNS"}

// probeResult 是某条线路的单轮探测结果。
type probeResult struct {
	mode     string
	addr     string
	upstream upstream.Upstream
	rtt      time.Duration
	ok       bool
	fresh    bool // true 表示本轮新建（live 未命中），探测后需按需回收
}

// Scheduler 周期性探测各家各模式的延迟，维护当前最优选路。
// live 按地址缓存存活 upstream，连续选中的线路跨周期复用热连接；
// signature 记录选路签名，未变化时不重建 config 以保留缓存；
// 只有退役线路才延迟关闭，给进行中的转发请求留收尾时间。
type Scheduler struct {
	cfg      config.Config
	logger   *slog.Logger
	opts     *upstream.Options
	probeMsg *dns.Msg

	// mu 保护 current（Handler 并发读）。
	mu      sync.RWMutex
	current *proxy.CustomUpstreamConfig

	// 以下字段仅在调度 goroutine 内读写，无需加锁。
	signature string
	live      map[string]upstream.Upstream
	done      chan struct{}
}

func New(cfg config.Config, logger *slog.Logger, opts *upstream.Options) *Scheduler {
	return &Scheduler{
		cfg:      cfg,
		logger:   logger,
		opts:     opts,
		probeMsg: newProbeMsg(cfg.ProbeDomain),
		current:  nil,
		live:     make(map[string]upstream.Upstream),
		done:     make(chan struct{}),
	}
}

// newProbeMsg 构造一条 A 记录查询，用于探测。
func newProbeMsg(domain string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeA)
	m.RecursionDesired = true
	return m
}

// Start 启动调度循环：先立即探测一轮，之后按周期探测，退出时关闭所有存活上游。
func (s *Scheduler) Start(ctx context.Context) {
	defer close(s.done)
	defer s.closeAllLive()

	s.probeAndSelect(ctx)

	ticker := time.NewTicker(s.cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeAndSelect(ctx)
		}
	}
}

// Stop 等待调度循环退出（退出时它自行关闭存活上游）。
func (s *Scheduler) Stop() {
	<-s.done
}

// CurrentConfig 返回当前选路对应的 custom upstream 配置；首轮探测未完成时返回 nil。
func (s *Scheduler) CurrentConfig() *proxy.CustomUpstreamConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *Scheduler) closeAllLive() {
	for _, u := range s.live {
		_ = u.Close()
	}
	s.live = make(map[string]upstream.Upstream)
}

// probeAndSelect 并发探测每家各模式延迟，每家选出延迟最低的模式。
func (s *Scheduler) probeAndSelect(ctx context.Context) {
	s.logger.Info("开始探测上游延迟")

	type providerProbe struct {
		name    string
		results []probeResult
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		probes []providerProbe
	)

	for name, modes := range s.cfg.DNS {
		wg.Add(1)
		go func(name string, modes map[string]string) {
			defer wg.Done()
			results := s.probeProvider(ctx, name, modes)
			mu.Lock()
			probes = append(probes, providerProbe{name: name, results: results})
			mu.Unlock()
		}(name, modes)
	}
	wg.Wait()

	// 每家保留延迟最低的模式，回收本轮新建但未被选中的对象。
	selected := make(map[string]*probeResult)
	for _, pp := range probes {
		best := pickBest(pp.results)
		if best == nil {
			s.logger.Warn("该服务商所有模式均不可用，本轮跳过", "provider", pp.name)
			closeFresh(pp.results, nil)
			continue
		}
		closeFresh(pp.results, best.upstream)
		selected[pp.name] = best

		s.logger.Info("选定最优线路",
			"provider", pp.name,
			"mode", best.mode,
			"addr", best.addr,
			"rtt", best.rtt.Round(time.Millisecond),
		)
	}

	selAddr := make(map[string]string, len(selected))
	selUp := make(map[string]upstream.Upstream, len(selected))
	for name, b := range selected {
		selAddr[name] = b.addr
		selUp[b.addr] = b.upstream
	}
	newSig := selectSignature(selAddr)

	if newSig == s.signature && s.current != nil {
		s.logger.Info("选路未变化，复用连接与缓存", "upstreams", len(selected))
		return
	}

	var newCfg *proxy.CustomUpstreamConfig
	if len(selUp) > 0 {
		// 所有上游共享同一个缓存实例：同一查询不因由哪家解析而异，且省内存。
		// 缓存跟随选路生命周期，选路变化时随 newCfg 一并重建。
		var shared *cache.Cache
		if *s.cfg.CacheEnabled {
			shared = cache.New(cache.Config{
				MaxBytes: int64(s.cfg.CacheSizeBytes),
				TTL:      s.cfg.CacheTTL,
				Eviction: cache.Policy(s.cfg.CacheEviction),
			})
		}
		ups := make([]upstream.Upstream, 0, len(selUp))
		for _, u := range selUp {
			ups = append(ups, s.wrapCached(u, shared))
		}
		// 缓存由 wrapping 的 cachingUpstream 承担（支持固定 TTL 与
		// FIFO/LRU/LFU 逐出），故此处关闭库自带的 per-route 缓存。
		newCfg = proxy.NewCustomUpstreamConfig(
			&proxy.UpstreamConfig{Upstreams: ups},
			false,
			0,
			false,
		)
	}

	s.mu.Lock()
	s.current = newCfg
	s.mu.Unlock()

	// 延迟 2×ProbeTimeout 关闭退役线路，让进行中的请求（超时上限 ProbeTimeout）先结束，避免 use-after-close。
	delay := 2 * s.cfg.ProbeTimeout
	for addr, u := range s.live {
		if _, keep := selUp[addr]; keep {
			continue
		}
		u := u
		time.AfterFunc(delay, func() { _ = u.Close() })
	}

	s.live = selUp
	s.signature = newSig

	s.logger.Info("本轮探测完成", "upstreams", len(selected))
}

// probeProvider 并发探测单个服务商的所有模式；优先复用 live 中的热连接，未命中才新建。
func (s *Scheduler) probeProvider(ctx context.Context, name string, modes map[string]string) []probeResult {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []probeResult
	)

	for _, mode := range modeOrder {
		addr, ok := modes[mode]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(mode, addr string) {
			defer wg.Done()

			var (
				u     upstream.Upstream
				fresh bool
				err   error
			)
			if existing := s.live[addr]; existing != nil {
				u = existing
			} else {
				u, err = upstream.AddressToUpstream(addr, s.opts)
				if err != nil {
					s.logger.Warn("解析上游失败", "provider", name, "mode", mode, "addr", addr, "err", err)
					return
				}
				fresh = true
			}

			rtt, ok := s.probeMode(ctx, u)
			mu.Lock()
			results = append(results, probeResult{mode: mode, addr: addr, upstream: u, rtt: rtt, ok: ok, fresh: fresh})
			mu.Unlock()
		}(mode, addr)
	}
	wg.Wait()

	return results
}

func closeFresh(results []probeResult, keep upstream.Upstream) {
	for _, r := range results {
		if r.fresh && r.upstream != nil && r.upstream != keep {
			_ = r.upstream.Close()
		}
	}
}

// probeMode 连续探测 ProbeCount 次，返回中位数 RTT；全部失败返回 ok=false。
func (s *Scheduler) probeMode(_ context.Context, u upstream.Upstream) (rtt time.Duration, ok bool) {
	var rtts []time.Duration

	for i := 0; i < s.cfg.ProbeCount; i++ {
		start := time.Now()
		_, err := u.Exchange(s.probeMsg)
		elapsed := time.Since(start)
		if err != nil {
			continue
		}
		rtts = append(rtts, elapsed)
	}

	if len(rtts) == 0 {
		return 0, false
	}

	return median(rtts), true
}

func selectSignature(selAddr map[string]string) string {
	names := make([]string, 0, len(selAddr))
	for n := range selAddr {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(selAddr[n])
		b.WriteByte(';')
	}
	return b.String()
}

// median 返回中位数；偶数个时取上中位数。
func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func pickBest(results []probeResult) *probeResult {
	var best *probeResult
	for i := range results {
		r := &results[i]
		if !r.ok {
			continue
		}
		if best == nil || r.rtt < best.rtt {
			best = r
		}
	}
	return best
}

// wrapCached 用共享缓存包装上游。shared 为 nil 表示缓存已关闭，直接返回原上游。
// 返回的 upstream.Upstream 会先查缓存，命中则直接返回、未命中才真正转发并回填。
//
// 之所以在「上游层」而非 handler 层做缓存：库的并行赛马（UpstreamModeParallel）
// 会逐一对每个上游调用 Exchange，缓存包装恰好在每次转发前拦截，天然复用了
// 库的去重、SERVFAIL 兜底与响应管线，无需在 handler 里重写整个解析流程。
func (s *Scheduler) wrapCached(u upstream.Upstream, shared *cache.Cache) upstream.Upstream {
	if shared == nil {
		return u
	}
	return &cachingUpstream{upstream: u, cache: shared}
}

// cachingUpstream 用共享缓存包装单个上游，实现 upstream.Upstream 接口。
// 所有上游共享同一个 cache 实例，保证同一查询跨上游命中。
type cachingUpstream struct {
	upstream upstream.Upstream
	cache    *cache.Cache
}

// Exchange 先查缓存，命中则返回副本；否则转发并把可缓存响应回填缓存。
func (c *cachingUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
	if resp := c.cache.Get(req); resp != nil {
		return resp, nil
	}
	resp, err := c.upstream.Exchange(req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		c.cache.Set(req, resp)
	}
	return resp, nil
}

// Address 透传底层上游地址。
func (c *cachingUpstream) Address() string { return c.upstream.Address() }

// Close 透传底层上游关闭。
func (c *cachingUpstream) Close() error { return c.upstream.Close() }

var _ upstream.Upstream = (*cachingUpstream)(nil)
