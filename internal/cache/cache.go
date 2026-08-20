// Package cache 实现 DNS 响应缓存，支持固定过期时间与 FIFO/LRU/LFU 三种逐出策略。
//
// 库（AdguardTeam/dnsproxy）自带的 per-route 缓存只支持 LRU 且 TTL 跟随记录自身，
// 无法满足「固定过期时间 + 可选逐出策略」的需求，故此处自建缓存，并在 scheduler
// 层以「共享缓存包装上游」的方式接入（见 internal/scheduler 的 cachingUpstream）。
package cache

import (
	"container/heap"
	"container/list"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Policy 是缓存满时的逐出策略。
type Policy string

const (
	// FIFO 先进先出：优先逐出最早插入的条目。
	FIFO Policy = "fifo"
	// LRU 最近最少使用：优先逐出最久未被访问的条目。
	LRU Policy = "lru"
	// LFU 最不经常使用：优先逐出访问次数最少的条目（次数相同则按插入先后）。
	LFU Policy = "lfu"
)

// Config 是缓存的配置。
type Config struct {
	// MaxBytes 是缓存的最大字节数（键 + 值的近似大小），<=0 表示不设上限。
	MaxBytes int64
	// TTL 是固定缓存过期时间；<=0 表示跟随记录自身的 TTL。
	TTL time.Duration
	// Eviction 是逐出策略，空值默认 LRU。
	Eviction Policy
}

// entry 是单条缓存记录，同时维护各逐出策略所需的元数据。
type entry struct {
	key    string
	msg    *dns.Msg
	size   int64
	expire time.Time

	// freq 是访问次数（LFU 用）。
	freq int64
	// seq 是单调递增的插入序号，作为 LFU 同频时的决胜条件。
	seq int64
	// elem 指向 FIFO/LRU 顺序链表中的节点；LFU 模式下为 nil。
	elem *list.Element
	// heapIdx 是 LFU 堆中的下标；非 LFU 模式下恒为 -1。
	heapIdx int
}

// Cache 是一个按字节上限约束、按策略逐出的线程安全缓存。
type Cache struct {
	mu  sync.Mutex
	cfg Config
	now func() time.Time

	items map[string]*entry
	total int64

	order *list.List // FIFO 与 LRU 共享的顺序链表，front 为逐出候选
	heap  lfuHeap    // LFU 的最小堆，堆顶为逐出候选
	seq   int64
}

// New 按配置构造缓存。
func New(cfg Config) *Cache {
	if cfg.Eviction == "" {
		cfg.Eviction = LRU
	}
	return &Cache{
		cfg:   cfg,
		now:   time.Now,
		items: make(map[string]*entry),
		order: list.New(),
	}
}

// Get 返回 req 的缓存响应副本；未命中或已过期返回 nil。
// 命中时会按剩余寿命改写响应各记录的 TTL，并把条目标记为「刚被访问」。
func (c *Cache) Get(req *dns.Msg) *dns.Msg {
	if req == nil || len(req.Question) != 1 {
		return nil
	}
	key := keyOf(req)

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		return nil
	}
	now := c.now()
	if now.After(e.expire) {
		c.removeEntry(e)
		return nil
	}

	c.touch(e)

	resp := e.msg.Copy()
	remaining := uint32(e.expire.Sub(now).Seconds())
	if remaining == 0 {
		remaining = 1
	}
	setTTL(resp, remaining)

	return resp
}

// Set 将 resp 缓存到 req 对应的键下；不可缓存的响应会被忽略。
func (c *Cache) Set(req, resp *dns.Msg) {
	if req == nil || resp == nil || len(req.Question) != 1 || !cacheable(resp) {
		return
	}

	ttl := c.cfg.TTL
	if ttl <= 0 {
		secs := recordTTL(resp)
		if secs == 0 {
			return
		}
		ttl = time.Duration(secs) * time.Second
	}

	key := keyOf(req)
	size := int64(resp.Len()) + int64(len(key))

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[key]; ok {
		c.replaceEntry(e, resp, size, ttl)
		return
	}
	c.evictFor(size)
	c.insertEntry(key, resp, size, ttl)
}

// Len 返回当前条目数。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Size 返回当前近似字节占用。
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Clear 清空缓存。
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*entry)
	c.total = 0
	c.order.Init()
	c.heap = nil
}

// insertEntry 在已持有锁的前提下插入新条目。
func (c *Cache) insertEntry(key string, msg *dns.Msg, size int64, ttl time.Duration) {
	e := &entry{
		key:     key,
		msg:     msg.Copy(),
		size:    size,
		expire:  c.now().Add(ttl),
		freq:    1,
		heapIdx: -1,
	}
	c.items[key] = e
	c.total += size

	switch c.cfg.Eviction {
	case FIFO, LRU:
		e.elem = c.order.PushBack(e)
	case LFU:
		c.seq++
		e.seq = c.seq
		heap.Push(&c.heap, e)
	}
}

// replaceEntry 在已持有锁的前提下用新值替换既有条目。
func (c *Cache) replaceEntry(e *entry, msg *dns.Msg, size int64, ttl time.Duration) {
	c.total += size - e.size
	e.msg = msg.Copy()
	e.size = size
	e.expire = c.now().Add(ttl)
	c.touch(e)
}

// touch 记录一次访问，用于维护 LRU 顺序或 LFU 频次。FIFO 下为空操作。
func (c *Cache) touch(e *entry) {
	switch c.cfg.Eviction {
	case LRU:
		if e.elem != nil {
			c.order.MoveToBack(e.elem)
		}
	case LFU:
		e.freq++
		if e.heapIdx >= 0 {
			heap.Fix(&c.heap, e.heapIdx)
		}
	}
}

// removeEntry 从 map、链表与堆中彻底移除条目，并扣减占用。已持有锁。
func (c *Cache) removeEntry(e *entry) {
	delete(c.items, e.key)
	c.total -= e.size
	if e.elem != nil {
		c.order.Remove(e.elem)
		e.elem = nil
	}
	if e.heapIdx >= 0 {
		heap.Remove(&c.heap, e.heapIdx)
		e.heapIdx = -1
	}
}

// evictFor 按逐出策略腾出至少 size 字节的空间。已持有锁。
func (c *Cache) evictFor(size int64) {
	if c.cfg.MaxBytes <= 0 {
		return
	}
	for len(c.items) > 0 && c.total+size > c.cfg.MaxBytes {
		c.evictOne()
	}
}

// evictOne 按策略逐出单个条目。已持有锁。
func (c *Cache) evictOne() {
	switch c.cfg.Eviction {
	case FIFO, LRU:
		front := c.order.Front()
		if front == nil {
			return
		}
		c.removeEntry(front.Value.(*entry))
	case LFU:
		if len(c.heap) == 0 {
			return
		}
		c.removeEntry(heap.Pop(&c.heap).(*entry))
	}
}

// lfuHeap 是 LFU 的最小堆：频次低、插入早的条目在堆顶。
type lfuHeap []*entry

func (h lfuHeap) Len() int { return len(h) }

func (h lfuHeap) Less(i, j int) bool {
	if h[i].freq != h[j].freq {
		return h[i].freq < h[j].freq
	}
	return h[i].seq < h[j].seq
}

func (h lfuHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *lfuHeap) Push(x any) {
	e := x.(*entry)
	e.heapIdx = len(*h)
	*h = append(*h, e)
}

func (h *lfuHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.heapIdx = -1
	*h = old[:n-1]
	return e
}

// keyOf 由请求构造缓存键：问题名（小写）+ QTYPE + QCLASS + DO 位 + ECS（若存在）。
// ECS 被纳入键，保证 pass 模式下不同子网不会串缓存。
func keyOf(m *dns.Msg) string {
	q := m.Question[0]
	var b strings.Builder
	b.Grow(len(q.Name) + 24)
	b.WriteString(strings.ToLower(q.Name))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(int(q.Qtype)))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(int(q.Qclass)))

	opt := m.IsEdns0()
	if opt == nil {
		return b.String()
	}
	if opt.Do() {
		b.WriteString("|D")
	}
	for _, o := range opt.Option {
		sn, ok := o.(*dns.EDNS0_SUBNET)
		if !ok {
			continue
		}
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(int(sn.Family)))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(int(sn.SourceNetmask)))
		b.WriteByte('/')
		b.Write(sn.Address)
	}
	return b.String()
}

// cacheable 判断响应是否可缓存：非截断、单问题，且 rcode 属于可缓存类别。
func cacheable(m *dns.Msg) bool {
	if m == nil || m.Truncated || len(m.Question) != 1 {
		return false
	}
	switch m.Rcode {
	case dns.RcodeSuccess:
		q := m.Question[0]
		return (q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA) || hasIPAns(m) || hasSOA(m)
	case dns.RcodeNameError:
		return hasSOA(m)
	case dns.RcodeServerFailure:
		return true
	default:
		return false
	}
}

// recordTTL 返回响应可缓存的秒数（各记录 TTL 的最小值，SERVFAIL 上限 30s）。
// 无可缓存记录返回 0。
func recordTTL(m *dns.Msg) uint32 {
	if m == nil || m.Truncated || len(m.Question) != 1 {
		return 0
	}
	ttl := ^uint32(0)
	for _, rrset := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrset {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			if rr.Header().Ttl < ttl {
				ttl = rr.Header().Ttl
			}
		}
	}
	if ttl == ^uint32(0) {
		return 0
	}
	if m.Rcode == dns.RcodeServerFailure && ttl > 30 {
		return 30
	}
	return ttl
}

// setTTL 将响应各记录的 TTL 统一改写为 ttl（跳过 OPT）。
func setTTL(m *dns.Msg, ttl uint32) {
	for _, rrset := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrset {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			rr.Header().Ttl = ttl
		}
	}
}

// hasIPAns 报告应答段是否含有 A 或 AAAA 记录。
func hasIPAns(m *dns.Msg) bool {
	for _, rr := range m.Answer {
		switch rr.Header().Rrtype {
		case dns.TypeA, dns.TypeAAAA:
			return true
		}
	}
	return false
}

// hasSOA 报告权威段是否含有 SOA 记录（用于负应答可缓存判定）。
func hasSOA(m *dns.Msg) bool {
	for _, rr := range m.Ns {
		if rr.Header().Rrtype == dns.TypeSOA {
			return true
		}
	}
	return false
}
