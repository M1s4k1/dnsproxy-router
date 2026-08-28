package scheduler

import (
	"sync"

	"github.com/miekg/dns"
)

// coalescer 合并进行中的相同查询（singleflight 语义）：按 key 分组，首个调用
// 执行 fn，其余等待并共享结果。用于缓存未命中回源与 stale 后台刷新时，避免
// 突发流量对同一域名向各家上游重复打重复查询。
type coalescer struct {
	mu sync.Mutex
	m  map[string]*coalesceCall
}

// coalesceCall 是一次进行中查询的状态，供后续等待者读取。
type coalesceCall struct {
	wg   sync.WaitGroup
	resp *dns.Msg
	err  error
}

// Do 返回 key 对应查询的结果：若已有相同 key 的查询在进行，则等待其完成并
// 返回结果副本；否则执行 fn 并记录结果，供并发等待者共享。
func (g *coalescer) Do(key string, fn func() (*dns.Msg, error)) (*dns.Msg, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*coalesceCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		if c.resp != nil {
			return c.resp.Copy(), nil
		}
		return nil, c.err
	}

	c := &coalesceCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	resp, err := fn()
	c.err = err
	if resp != nil {
		// 快照一份供等待者读取：发起者返回的原始 resp 会被上层（dnsproxy）
		// 继续修改（filter/scrub），若等待者直接读同一指针会数据竞争。
		c.resp = resp.Copy()
	}
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	// 发起者返回原始 resp（供其上层继续处理），等待者读的是上面的快照副本。
	return resp, err
}
