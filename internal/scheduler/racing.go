package scheduler

import (
	"sort"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"dnsproxy-scheduler/internal/cache"
)

// racingMember 是参与赛马的一个上游及其元信息。
type racingMember struct {
	name     string
	weight   int
	upstream upstream.Upstream
}

// racingUpstream 是一个聚合上游：把「多家当前最优线路」收拢为一个 upstream，
// 交给库的单元素 exchange 路径（UpstreamModeParallel 对单元素直接调 Exchange）。
// 由它内部并发查询所有子上游，按模式选出返回结果：
//   - fastest：谁先成功返回用谁（等价于库的并发赛马）；
//   - weighted：第一个成功响应后的窗口期内收集所有成功响应，取权重最高者，
//     权重相同时取响应最快者。
type racingUpstream struct {
	members  []racingMember
	window   time.Duration
	weighted bool
}

// racingResult 是单个上游的并发查询结果。
type racingResult struct {
	weight   int
	upstream upstream.Upstream
	resp     *dns.Msg
	err      error
	elapsed  time.Duration
}

// newRacing 构造聚合上游。weighted 为 false 表示 fastest 模式（window 忽略）。
// members 按权重从高到低、权重相同时按名字字典序预排序，保证选择稳定。
func newRacing(members []racingMember, weighted bool, window time.Duration) *racingUpstream {
	sort.Slice(members, func(i, j int) bool {
		if members[i].weight != members[j].weight {
			return members[i].weight > members[j].weight
		}
		return members[i].name < members[j].name
	})
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

// exchangeWeighted 第一个成功响应后等待窗口期，收集窗口内所有成功响应，
// 取权重最高者；权重相同时取响应最快者。
func (r *racingUpstream) exchangeWeighted(req *dns.Msg) (*dns.Msg, error) {
	resCh := make(chan racingResult, len(r.members))
	for _, m := range r.members {
		go func(m racingMember) {
			start := time.Now()
			resp, err := m.upstream.Exchange(req.Copy())
			resCh <- racingResult{weight: m.weight, resp: resp, err: err, elapsed: time.Since(start)}
		}(m)
	}

	var (
		windowEnd time.Time
		windowSet bool
		cands     []racingResult
	)

	for range r.members {
		res := <-resCh
		if res.err != nil || res.resp == nil {
			continue
		}
		if !windowSet {
			windowEnd = time.Now().Add(r.window)
			windowSet = true
			cands = append(cands, res)
			continue
		}
		if time.Now().Before(windowEnd) {
			cands = append(cands, res)
		}
	}

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
