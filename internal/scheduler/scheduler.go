// Package scheduler 周期性探测各家各模式的延迟，并维护当前的最优选路。
package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"dnsproxy-router/internal/afp"
	"dnsproxy-router/internal/cache"
	"dnsproxy-router/internal/config"
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
	probeMsg *dns.Msg

	// providerOpts 为每个服务商的专属上游选项（Clone 自 base，按优先级改写）。
	// 仅在调度 goroutine 内只读，无需加锁。
	providerOpts map[string]*upstream.Options
	// probers 为 latency 服务商的延迟探测器，Start 时统一启动。
	probers []*afp.Prober

	// mu 保护 current（Handler 并发读）。
	mu      sync.RWMutex
	current *proxy.CustomUpstreamConfig

	// 以下字段仅在调度 goroutine 内读写，无需加锁。
	signature string
	live      map[string]upstream.Upstream
	done      chan struct{}

	// cancel 取消内部运行 context，由 Start 设置；cancelMu 保护其并发读写。
	// grace 是 Retire 写入的优雅关停延迟（纳秒）：>0 时 Start 退出后延迟关闭
	// 存活上游，给在途请求收尾时间；0 表示立即关闭。
	// started 在 Start 写完 cancel 后关闭，供 Retire/Stop 等待，避免读到 nil
	// cancel 而把取消静默丢弃；firstDone 在首轮探测完成时关闭，供热重载等待
	// 新调度器完成首轮选路后再切换（否则切换后请求会回退占位上游）。
	cancelMu  sync.Mutex
	cancel    context.CancelFunc
	grace     atomic.Int64
	started   chan struct{}
	firstDone chan struct{}
}

// New 构造调度器。base 为共享的基础上游选项，其 Bootstrap 是未包 Selector 的
// 引导解析链；每个服务商按其地址族优先级 Clone 出专属 opts。
func New(cfg config.Config, logger *slog.Logger, base *upstream.Options) *Scheduler {
	baseResolver := base.Bootstrap

	s := &Scheduler{
		cfg:          cfg,
		logger:       logger,
		probeMsg:     newProbeMsg(cfg.ProbeDomain),
		providerOpts: make(map[string]*upstream.Options, len(cfg.DNS)),
		current:      nil,
		live:         make(map[string]upstream.Upstream),
		done:         make(chan struct{}),
		started:      make(chan struct{}),
		firstDone:    make(chan struct{}),
	}

	for name := range cfg.DNS {
		opts := base.Clone()
		switch cfg.IPPriorityFor(name) {
		case "ipv6":
			opts.PreferIPv6 = true
		case "latency":
			targets := afpTargetsOf(cfg.UpstreamTargetsFor(name))
			// 端点全为 IP 字面量时无法测两族延迟，退化为 IPv4 静态偏好。
			if len(targets) == 0 {
				logger.Warn("latency 服务商无可探测主机名，退化为 IPv4 优先", "provider", name)
				opts.PreferIPv6 = false
				break
			}
			prober := afp.NewProber(logger, baseResolver, targets, time.Duration(cfg.IPLatencyInterval))
			opts.Bootstrap = afp.NewSelector(baseResolver, afp.IPv4, prober)
			opts.PreferIPv6 = false
			s.probers = append(s.probers, prober)
		default: // "ipv4"
			opts.PreferIPv6 = false
		}
		s.providerOpts[name] = opts
	}

	return s
}

// afpTargetsOf 把 config 的探测目标转成 afp.Target。
func afpTargetsOf(ts []config.UpstreamTarget) []afp.Target {
	targets := make([]afp.Target, 0, len(ts))
	for _, t := range ts {
		targets = append(targets, afp.Target{Host: t.Host, Port: t.Port})
	}
	return targets
}

// newProbeMsg 构造一条 A 记录查询，用于探测。
func newProbeMsg(domain string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeA)
	m.RecursionDesired = true
	return m
}

// Start 启动调度循环：先立即探测一轮，之后按周期探测，退出时关闭所有存活上游。
// 同时启动所有 per-provider 延迟探测器，并在退出前等待它们结束（保证 Stop 语义）。
// parent 是进程级 context；调度器在内部派生可取消子 context，供 Stop/Retire 触发。
func (s *Scheduler) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()
	close(s.started) // 通知 cancel 已就绪；Retire/Stop 等待此信号后再取 cancel

	var wg sync.WaitGroup
	for _, p := range s.probers {
		wg.Add(1)
		go func(p *afp.Prober) {
			defer wg.Done()
			p.Start(ctx)
		}(p)
	}

	// defer 为 LIFO：执行顺序为 关存活上游（立即或延迟 grace）→ wg.Wait() →
	// close(done)。这样 Stop() 返回即代表调度循环与所有 prober 均已退出。
	defer func() { wg.Wait(); close(s.done) }()
	defer func() {
		if d := s.grace.Load(); d > 0 {
			time.AfterFunc(time.Duration(d), s.closeAllLive)
		} else {
			s.closeAllLive()
		}
	}()

	s.probeAndSelect(ctx)
	close(s.firstDone) // 首轮探测完成（无论选路是否就绪）

	ticker := time.NewTicker(time.Duration(s.cfg.ProbeInterval))
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

// Stop 触发取消并等待调度循环退出（退出时它自行关闭存活上游）。
func (s *Scheduler) Stop() {
	<-s.started // 等 Start 写完 cancel，避免读到 nil 取消
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-s.done
}

// Retire 触发调度循环退出，但把存活上游的关闭延迟 delay 时间，给在途请求收尾。
// 不阻塞；用于热重载时旧调度器优雅退役（新调度器接管后立即返回）。
func (s *Scheduler) Retire(delay time.Duration) {
	<-s.started // 等 Start 写完 cancel，避免读到 nil 取消
	s.grace.Store(int64(delay))
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// FirstDone 返回首轮探测完成的通知（无论选路是否就绪）。
// 供热重载使用：新调度器完成首轮选路后再切换，避免切换后请求回退占位上游。
func (s *Scheduler) FirstDone() <-chan struct{} {
	return s.firstDone
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
	members := make([]racingMember, 0, len(selected))
	for name, b := range selected {
		selAddr[name] = b.addr
		selUp[name+"\x00"+b.addr] = b.upstream
		members = append(members, racingMember{
			weight:   s.cfg.Weight(name),
			upstream: b.upstream,
		})
	}
	newSig := selectSignature(selAddr)

	if newSig == s.signature && s.current != nil {
		s.logger.Info("选路未变化，复用连接与缓存", "upstreams", len(selected))
		return
	}

	// 预热最终选中的各家上游：触发 DoH 客户端的 HTTP/3 竞速建连与连接池
	// 初始化，避免首个真实请求承担建连延迟。探测阶段的 probeMode 已对全部
	// 模式建连，此处仅对最终选路显式预热一次（热连接上近乎零成本）。
	s.warmup(members)

	var newCfg *proxy.CustomUpstreamConfig
	if len(members) > 0 {
		// 把「多家当前最优线路」收拢为一个聚合上游，交给库的单元素 exchange
		// 路径（UpstreamModeParallel 对单元素直接调 Exchange），由聚合上游内部
		// 并发查询并实现 fastest（谁先成功用谁）或 weighted（加权 + 延时窗口）。
		weighted := s.cfg.UpstreamMode == "weighted"
		racing := newRacing(members, weighted, time.Duration(s.cfg.RaceWindow))

		// 熔断：连续失败达到阈值的成员在冷却期内不参与查询，实现周期内快速失败。
		if s.cfg.BreakerFailThreshold > 0 {
			racing.breaker = newBreaker(len(members), s.cfg.BreakerFailThreshold,
				time.Duration(s.cfg.BreakerCooldown), s.logger)
		}

		// 缓存包在聚合层外层：一个请求先查缓存，未命中才并发查所有子并回填，
		// 避免各上游重复查询。共享同一实例，跟随选路生命周期，选路变化时重建。
		var finalUp upstream.Upstream = racing
		if *s.cfg.CacheEnabled {
			shared := cache.New(cache.Config{
				MaxBytes:       int64(s.cfg.CacheSizeBytes),
				TTL:            time.Duration(*s.cfg.CacheTTL),
				Eviction:       cache.Policy(s.cfg.CacheEviction),
				StaleMaxAge:    time.Duration(s.cfg.CacheStaleMaxAge),
				StaleAnswerTTL: time.Duration(s.cfg.CacheStaleAnswerTTL),
				MinTTL:         time.Duration(s.cfg.TTLMin),
				MaxTTL:         time.Duration(s.cfg.TTLMax),
			})
			finalUp = s.wrapCached(racing, shared)
		}

		// 缓存由 wrapping 的 cachingUpstream 承担（支持固定 TTL 与
		// FIFO/LRU/LFU 逐出），故此处关闭库自带的 per-route 缓存。
		newCfg = proxy.NewCustomUpstreamConfig(
			&proxy.UpstreamConfig{Upstreams: []upstream.Upstream{finalUp}},
			false,
			0,
			false,
		)
	}

	s.mu.Lock()
	s.current = newCfg
	s.mu.Unlock()

	// 延迟 2×ProbeTimeout 关闭退役线路，让进行中的请求（超时上限 ProbeTimeout）先结束，避免 use-after-close。
	delay := 2 * time.Duration(s.cfg.ProbeTimeout)
	for key, u := range s.live {
		if _, keep := selUp[key]; keep {
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
			key := name + "\x00" + addr
			if existing := s.live[key]; existing != nil {
				u = existing
			} else {
				u, err = upstream.AddressToUpstream(addr, s.providerOpts[name])
				if err != nil {
					s.logger.Warn("解析上游失败", "provider", name, "mode", mode, "addr", addr, "err", err)
					return
				}
				fresh = true
			}

			rtt, perr := s.probeMode(ctx, u)
			if perr != nil {
				s.logger.Warn("探测失败", "provider", name, "mode", mode, "addr", addr, "err", perr)
			}
			mu.Lock()
			results = append(results, probeResult{mode: mode, addr: addr, upstream: u, rtt: rtt, ok: perr == nil, fresh: fresh})
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

// probeMode 连续探测 ProbeCount 次，返回成功子集的中位数 RTT。容忍单次抖动：
// 只要有一次成功即算可用；全部失败才返回首个错误，供上层打日志定位
// （此前失败被静默吞掉，排障困难）。
func (s *Scheduler) probeMode(_ context.Context, u upstream.Upstream) (rtt time.Duration, err error) {
	// 各 provider/mode 的探测并发进行，Exchange 可能改写请求，故每 goroutine 用副本。
	msg := s.probeMsg.Copy()
	var rtts []time.Duration
	var firstErr error

	for i := 0; i < s.cfg.ProbeCount; i++ {
		start := time.Now()
		_, err := u.Exchange(msg)
		elapsed := time.Since(start)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		rtts = append(rtts, elapsed)
	}

	if len(rtts) == 0 {
		return 0, firstErr
	}

	return median(rtts), nil
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

// wrapCached 用共享缓存包装上游；shared 为 nil 表示缓存已关闭，直接返回原上游。
func (s *Scheduler) wrapCached(u upstream.Upstream, shared *cache.Cache) upstream.Upstream {
	if shared == nil {
		return u
	}
	return &cachingUpstream{upstream: u, cache: shared}
}

// warmup 对最终选中的各家上游并发做一次预热查询，触发 DoH 客户端的
// HTTP/3 竞速建连与连接池初始化，避免首个真实请求承担建连延迟。预热失败
// 不影响选路（探测阶段已判可用），仅记日志便于排障。
func (s *Scheduler) warmup(members []racingMember) {
	if len(members) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(m racingMember) {
			defer wg.Done()
			start := time.Now()
			_, err := m.upstream.Exchange(s.probeMsg.Copy())
			if err != nil {
				s.logger.Debug("预热失败", "upstream", m.upstream.Address(), "err", err)
				return
			}
			s.logger.Debug("预热完成", "upstream", m.upstream.Address(), "elapsed", time.Since(start).Round(time.Millisecond))
		}(m)
	}
	wg.Wait()
}
