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

	"dnsproxy-scheduler/internal/config"
)

// modeOrder 定义探测时模式的稳定遍历顺序，避免 map 无序导致日志抖动。
var modeOrder = []string{"DNS-over-HTTPS", "DNS-over-TLS", "DNS-over-QUIC"}

// probeResult 是某条线路的单轮探测结果。
type probeResult struct {
	mode     string
	addr     string
	upstream upstream.Upstream
	rtt      time.Duration
	ok       bool
	fresh    bool // true 表示本轮新建（live 未命中），探测后需按需回收
}

// Scheduler 负责周期性探测各家各模式的延迟，并维护当前的最优选路。
//
// 资源复用策略：
//   - live 按地址索引存活的 upstream 对象。只要某条线路连续被选中，其
//     连接对象（含热 TLS/QUIC/TCP 连接池）就跨周期复用，避免每 15 分钟
//     周期边界的全量冷启动重连。
//   - signature 记录当前选路签名。选路未变化时连 CustomUpstreamConfig 都
//     不重建，缓存（cache）也得以跨周期保留。
//   - 只有真正退役的线路（上轮选中、本轮不再选中）才会延迟关闭，给可能
//     仍在进行的转发请求留出收尾时间。
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

// New 构造一个 Scheduler。
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

// Start 启动调度循环：先立即探测一轮，之后按周期探测。
// 退出时在自身 goroutine 内关闭所有存活上游。
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

// CurrentConfig 返回当前选路对应的 custom upstream 配置（供 Handler 注入）。
// 尚未完成首轮探测时返回 nil。
func (s *Scheduler) CurrentConfig() *proxy.CustomUpstreamConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// closeAllLive 关闭所有存活上游，用于进程退出时的优雅关闭。
func (s *Scheduler) closeAllLive() {
	for _, u := range s.live {
		_ = u.Close()
	}
	s.live = make(map[string]upstream.Upstream)
}

// probeAndSelect 对每家各模式测延迟，每家选出延迟最低的模式。
// 探测并发进行，以避免首轮就绪过慢。
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

	// 汇总：每家保留延迟最低的模式，并回收本轮新建但未被选中的对象。
	selected := make(map[string]*probeResult) // name -> best
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

	// 选路签名：名称与地址的稳定映射。
	selAddr := make(map[string]string, len(selected))
	selUp := make(map[string]upstream.Upstream, len(selected))
	for name, b := range selected {
		selAddr[name] = b.addr
		selUp[b.addr] = b.upstream
	}
	newSig := selectSignature(selAddr)

	// 选路未变化（且已有有效配置）→ 直接复用连接与缓存，不做任何重建。
	if newSig == s.signature && s.current != nil {
		s.logger.Info("选路未变化，复用连接与缓存", "upstreams", len(selected))
		return
	}

	// 选路变化：构建新配置并原子替换。
	var newCfg *proxy.CustomUpstreamConfig
	if len(selUp) > 0 {
		ups := make([]upstream.Upstream, 0, len(selUp))
		for _, u := range selUp {
			ups = append(ups, u)
		}
		newCfg = proxy.NewCustomUpstreamConfig(
			&proxy.UpstreamConfig{Upstreams: ups},
			*s.cfg.CacheEnabled, // 按配置决定是否启用缓存
			s.cfg.CacheSizeBytes,
			false,
		)
	}

	s.mu.Lock()
	s.current = newCfg
	s.mu.Unlock()

	// 延迟关闭真正退役的线路（上轮选中、本轮不再选中）。
	// 延迟 2×ProbeTimeout：保证已拿到旧配置、仍在进行中的请求（其 Exchange
	// 超时上限为 ProbeTimeout）全部结束后再关闭底层连接，避免 use-after-close。
	delay := 2 * s.cfg.ProbeTimeout
	for addr, u := range s.live {
		if _, keep := selUp[addr]; keep {
			continue // 该线路仍被选中，继续复用
		}
		u := u
		time.AfterFunc(delay, func() { _ = u.Close() })
	}

	// 更新存活表与签名。
	s.live = selUp
	s.signature = newSig

	s.logger.Info("本轮探测完成", "upstreams", len(selected))
}

// probeProvider 并发探测单个服务商的所有模式，返回各模式结果。
// 优先复用 live 中的存活对象以保持热连接；live 未命中才新建。
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

// closeFresh 关闭一批本轮新建且未成为 keep 的对象（避免连接泄漏）。
func closeFresh(results []probeResult, keep upstream.Upstream) {
	for _, r := range results {
		if r.fresh && r.upstream != nil && r.upstream != keep {
			_ = r.upstream.Close()
		}
	}
}

// probeMode 对单个上游连续探测 ProbeCount 次，返回中位数 RTT。
// 全部失败时返回 ok=false。
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

// selectSignature 生成选路签名：按服务商名排序后拼接 name=addr。
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

// median 返回中位数；偶数个时取上中位数（偏大的那个）。
func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// pickBest 从结果里选出延迟最低的成功项；全部失败返回 nil。
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
