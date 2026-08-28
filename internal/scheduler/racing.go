package scheduler

import (
	"sort"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"dnsproxy-router/internal/cache"
)

// racingMember 是参与赛马的一个上游及其权重。
type racingMember struct {
	weight   int
	upstream upstream.Upstream
}

// racingUpstream 把「多家当前最优线路」收拢为一个上游，交给库的单元素 exchange
// 路径（UpstreamModeParallel 对单元素直接调 Exchange）。由它内部并发查询所有子上游：
// fastest 谁先成功返回用谁；weighted 在首个成功响应后的窗口期内收集成功响应，
// 取权重最高者，权重相同时取响应最快者。
type racingUpstream struct {
	members  []racingMember
	window   time.Duration
	weighted bool
}

// racingResult 是单个上游的并发查询结果。
type racingResult struct {
	weight  int
	resp    *dns.Msg
	err     error
	elapsed time.Duration
}

func newRacing(members []racingMember, weighted bool, window time.Duration) *racingUpstream {
	return &racingUpstream{members: members, weighted: weighted, window: window}
}

// Exchange 并发查询所有子上游并按模式返回结果。
func (r *racingUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
	switch {
	case len(r.members) == 0:
		return nil, upstream.ErrNoUpstreams
	case len(r.members) == 1:
		return r.members[0].upstream.Exchange(req)
	case r.weighted:
		return r.exchangeWeighted(req)
	default:
		return r.exchangeFastest(req)
	}
}

// exchangeFastest 谁先成功返回用谁（所有上游均失败则返回错误）。
func (r *racingUpstream) exchangeFastest(req *dns.Msg) (*dns.Msg, error) {
	resCh := make(chan racingResult, len(r.members))
	for _, m := range r.members {
		go func(m racingMember) {
			start := time.Now()
			resp, err := m.upstream.Exchange(req.Copy())
			resCh <- racingResult{weight: m.weight, resp: resp, err: err, elapsed: time.Since(start)}
		}(m)
	}

	var lastErr error
	for range r.members {
		res := <-resCh
		if res.err == nil && res.resp != nil {
			return res.resp, nil
		}
		if res.err != nil {
			lastErr = res.err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, upstream.ErrNoReply
}

// exchangeWeighted 在首个成功响应后开启窗口期，收集窗口内的所有成功响应，
// 窗口一到即返回，不再等待尚未返回的上游。
func (r *racingUpstream) exchangeWeighted(req *dns.Msg) (*dns.Msg, error) {
	resCh := make(chan racingResult, len(r.members))
	for _, m := range r.members {
		go func(m racingMember) {
			start := time.Now()
			resp, err := m.upstream.Exchange(req.Copy())
			resCh <- racingResult{weight: m.weight, resp: resp, err: err, elapsed: time.Since(start)}
		}(m)
	}

	var cands []racingResult
	var timer *time.Timer
	remaining := len(r.members)

	for remaining > 0 {
		if timer == nil {
			res := <-resCh
			remaining--
			if res.err == nil && res.resp != nil {
				cands = append(cands, res)
				timer = time.NewTimer(r.window)
			}
			continue
		}

		select {
		case res := <-resCh:
			remaining--
			if res.err == nil && res.resp != nil {
				cands = append(cands, res)
			}
		case <-timer.C:
			return pickWeighted(cands)
		}
	}

	if timer != nil {
		timer.Stop()
	}
	return pickWeighted(cands)
}

// pickWeighted 从候选中取权重最高者，权重相同时取响应最快者；候选为空返回 ErrNoReply。
func pickWeighted(cands []racingResult) (*dns.Msg, error) {
	if len(cands) == 0 {
		return nil, upstream.ErrNoReply
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].weight != cands[j].weight {
			return cands[i].weight > cands[j].weight
		}
		return cands[i].elapsed < cands[j].elapsed
	})
	return cands[0].resp, nil
}

// Address 返回空地址（聚合上游无单一地址）。
func (r *racingUpstream) Address() string { return "" }

// Close 关闭所有子上游。实际运行中 s.current 不被库关闭，此方法仅作语义兜底。
func (r *racingUpstream) Close() error {
	var firstErr error
	for _, m := range r.members {
		if err := m.upstream.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var _ upstream.Upstream = (*racingUpstream)(nil)

// cachingUpstream 在聚合上游外层加一层响应缓存，并附带两项优化：
//   - 请求合并：相同查询并发到达时，只向聚合上游发一份，其余等待共享，避免
//     缓存未命中时的回源放大（singleflight 语义）。
//   - 乐观缓存（stale-while-revalidate）：命中 stale 条目时先返回旧答案，再
//     后台异步回源刷新，消除「过期瞬间缓存击穿」与回源延迟。
type cachingUpstream struct {
	upstream upstream.Upstream
	cache    *cache.Cache
	coalesce coalescer
}

// Exchange 先查缓存，命中则返回副本；否则经请求合并回源并把可缓存响应回填。
func (c *cachingUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
	// 单问题之外的请求无法形成稳定缓存键，直接透传、不参与缓存与合并。
	key := cache.Key(req)
	if key == "" {
		return c.upstream.Exchange(req)
	}

	if resp := c.cache.Get(req); resp != nil {
		return resp, nil
	}

	// 命中 stale 条目：先返回旧答案（短 TTL），再后台刷新。
	if resp, ok := c.cache.GetStale(req); ok {
		c.refreshAsync(key, req)
		return resp, nil
	}

	return c.coalesce.Do(key, func() (*dns.Msg, error) {
		resp, err := c.upstream.Exchange(req)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			// TTL 覆写：钳制后的 TTL 既决定返回给客户端的可见值，也（在跟随
			// 记录 TTL 模式下）决定缓存过期时间。先钳制再缓存，保证两者一致。
			c.cache.Clamp(resp)
			c.cache.Set(req, resp)
		}
		return resp, nil
	})
}

// refreshAsync 后台异步回源刷新 stale 条目；期间同一 key 只发一份。刷新结果
// 只用于回填缓存，不阻塞当前请求，故忽略返回与错误（错误由回源路径自身日志）。
func (c *cachingUpstream) refreshAsync(key string, req *dns.Msg) {
	// 快照请求：异步 goroutine 在 Exchange 返回后仍持有 req，而上层可能已
	// 修改或复用原始 req，必须隔离，避免数据竞争。
	reqCopy := req.Copy()
	go c.coalesce.Do(key, func() (*dns.Msg, error) {
		resp, err := c.upstream.Exchange(reqCopy)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			c.cache.Clamp(resp)
			c.cache.Set(reqCopy, resp)
		}
		return resp, nil
	})
}

func (c *cachingUpstream) Address() string { return c.upstream.Address() }
func (c *cachingUpstream) Close() error    { return c.upstream.Close() }

var _ upstream.Upstream = (*cachingUpstream)(nil)
