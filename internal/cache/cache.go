// Package cache 实现 DNS 响应缓存，支持固定过期时间与 FIFO/LRU/LFU 三种逐出策略。
// 库自带的 per-route 缓存只支持 LRU 且 TTL 跟随记录自身，无法满足需求，故自建。
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
	// StaleMaxAge 是条目过期后仍保留的最长时长（乐观缓存窗口）。过期未超此
	// 窗口的条目可由 [Cache.GetStale] 返回、并触发后台回源；<=0 表示不启用
	// 乐观缓存，过期即删除。
	StaleMaxAge time.Duration
	// StaleAnswerTTL 是命中 stale 条目时返回给客户端的短 TTL；<=0 用默认 30s。
	StaleAnswerTTL time.Duration
	// MinTTL 是上游记录 TTL 的下限钳制：低于此值拉高（如上游返回 0-TTL 时拉高
	// 以便缓存）；0 表示不钳制。
	MinTTL time.Duration
	// MaxTTL 是上游记录 TTL 的上限钳制：高于此值压低（防止缓存污染过久）；
	// 0 表示不钳制。
	MaxTTL time.Duration
}

// entry 是单条缓存记录，同时维护各逐出策略所需的元数据。
type entry struct {
	key    string
	msg    *dns.Msg
	size   int64
	expire time.Time
	// staleUntil 是条目彻底删除的时间点：过期（expire）后、未超 staleUntil
	// 前，条目仍在、可由 GetStale 返回。未启用乐观缓存时恒等于 expire。
	staleUntil time.Time

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
	if cfg.StaleAnswerTTL <= 0 {
		cfg.StaleAnswerTTL = 30 * time.Second
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
// 已过期但仍在 stale 窗口内的条目不会被删除，可由 GetStale 返回。
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
		if now.After(e.staleUntil) {
			c.removeEntry(e)
		}
		return nil
	}

	c.touch(e)

	resp := e.msg.Copy()
	remaining := uint32(e.expire.Sub(now).Seconds())
	if remaining == 0 {
		remaining = 1
	}
	// 钳上限：固定 TTL 模式下过期时间可能大于 MaxTTL，返回给客户端的剩余
	// 寿命仍需不超过 MaxTTL。下限不钳——命中的剩余寿命随递减低于 MinTTL 属
	// 正常 DNS 语义（MinTTL 只在写入时保证至少缓存这么久）。
	if max := c.maxTTL(); max > 0 && remaining > max {
		remaining = max
	}
	setTTL(resp, remaining)

	return resp
}

// GetStale 返回已过期但仍在 stale 窗口内的条目响应副本，并把其 TTL 改写为
// 短 StaleAnswerTTL，供「过期即回源」（stale-while-revalidate）使用。返回的
// ok 为 true 表示命中 stale 条目；未启用乐观缓存、未命中或已彻底过期返回 false。
func (c *Cache) GetStale(req *dns.Msg) (resp *dns.Msg, ok bool) {
	if req == nil || len(req.Question) != 1 || c.cfg.StaleMaxAge <= 0 {
		return nil, false
	}
	key := keyOf(req)

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	now := c.now()
	if now.Before(e.expire) {
		// 仍新鲜，应走 Get 路径，此处不返回。
		return nil, false
	}
	if now.After(e.staleUntil) {
		c.removeEntry(e)
		return nil, false
	}

	resp = e.msg.Copy()
	ttl := uint32(c.cfg.StaleAnswerTTL.Seconds())
	if ttl == 0 {
		ttl = 1
	}
	if max := c.maxTTL(); max > 0 && ttl > max {
		ttl = max
	}
	setTTL(resp, ttl)

	return resp, true
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

// Clamp 原地把 resp 各记录（跳过 OPT）的 TTL 钳制到 [MinTTL, MaxTTL]，返回 resp
// 本身。供回源路径在「返回给客户端」与「写入缓存」之前调用：钳制后的 TTL 既
// 决定客户端可见 TTL，也（在跟随记录 TTL 模式下）决定缓存过期时间。未启用钳制
// （MinTTL、MaxTTL 均为 0）时直接返回原 resp，零开销。
func (c *Cache) Clamp(resp *dns.Msg) *dns.Msg {
	if resp == nil {
		return resp
	}
	min, max := c.minTTL(), c.maxTTL()
	if min == 0 && max == 0 {
		return resp
	}
	for _, rrset := range [][]dns.RR{resp.Answer, resp.Ns, resp.Extra} {
		for _, rr := range rrset {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			ttl := rr.Header().Ttl
			if min > 0 && ttl < min {
				ttl = min
			}
			if max > 0 && ttl > max {
				ttl = max
			}
			rr.Header().Ttl = ttl
		}
	}
	return resp
}

// minTTL 返回 MinTTL 的秒数（0 表示未启用）。
func (c *Cache) minTTL() uint32 { return uint32(c.cfg.MinTTL / time.Second) }

// maxTTL 返回 MaxTTL 的秒数（0 表示未启用）。
func (c *Cache) maxTTL() uint32 { return uint32(c.cfg.MaxTTL / time.Second) }

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
	now := c.now()
	e := &entry{
		key:        key,
		msg:        msg.Copy(),
		size:       size,
		expire:     now.Add(ttl),
		staleUntil: now.Add(ttl + c.cfg.StaleMaxAge),
		freq:       1,
		heapIdx:    -1,
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
	now := c.now()
	e.expire = now.Add(ttl)
	e.staleUntil = now.Add(ttl + c.cfg.StaleMaxAge)
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

// Key 返回 req 的缓存键；req 必须为单问题，否则返回空串。供缓存层之外的
// 调用方（如请求合并）复用与缓存一致的键，保证合并与缓存命中对齐。
func Key(m *dns.Msg) string {
	if m == nil || len(m.Question) != 1 {
		return ""
	}
	return keyOf(m)
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
