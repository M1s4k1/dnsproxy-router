package cache

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func makeReq(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	return m
}

func makeResp(req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.IPv4(1, 2, 3, 4),
	}}
	return m
}

// 三个等长域名，保证单条 size 一致，便于精确控制容量。
const (
	nameA = "a.example.com."
	nameB = "b.example.com."
	nameC = "c.example.com."
)

func oneSize() int64 {
	req := makeReq(nameA)
	resp := makeResp(req)
	return int64(resp.Len()) + int64(len(keyOf(req)))
}

// newTestCache 构造一个只能容纳两条的缓存。
func newTestCache(p Policy) *Cache {
	return New(Config{MaxBytes: 2 * oneSize(), TTL: time.Minute, Eviction: p})
}

func TestFIFOEviction(t *testing.T) {
	c := newTestCache(FIFO)
	reqA, reqB, reqC := makeReq(nameA), makeReq(nameB), makeReq(nameC)
	c.Set(reqA, makeResp(reqA))
	c.Set(reqB, makeResp(reqB))
	c.Set(reqC, makeResp(reqC))

	if c.Get(reqA) != nil {
		t.Fatal("FIFO 应逐出最早插入的 a")
	}
	if c.Get(reqB) == nil || c.Get(reqC) == nil {
		t.Fatal("FIFO 应保留 b、c")
	}
}

func TestLRUEviction(t *testing.T) {
	c := newTestCache(LRU)
	reqA, reqB, reqC := makeReq(nameA), makeReq(nameB), makeReq(nameC)
	c.Set(reqA, makeResp(reqA))
	c.Set(reqB, makeResp(reqB))

	// 访问 a，使 b 成为最久未用。
	if c.Get(reqA) == nil {
		t.Fatal("a 应命中")
	}
	c.Set(reqC, makeResp(reqC))

	if c.Get(reqB) != nil {
		t.Fatal("LRU 应逐出最久未用的 b")
	}
	if c.Get(reqA) == nil || c.Get(reqC) == nil {
		t.Fatal("LRU 应保留 a、c")
	}
}

func TestLFUEviction(t *testing.T) {
	c := newTestCache(LFU)
	reqA, reqB, reqC := makeReq(nameA), makeReq(nameB), makeReq(nameC)
	c.Set(reqA, makeResp(reqA))
	c.Set(reqB, makeResp(reqB))

	// 多次访问 a，使 a 频次高于 b。
	c.Get(reqA)
	c.Get(reqA)
	c.Set(reqC, makeResp(reqC))

	if c.Get(reqB) != nil {
		t.Fatal("LFU 应逐出频次最低的 b")
	}
	if c.Get(reqA) == nil || c.Get(reqC) == nil {
		t.Fatal("LFU 应保留 a、c")
	}
}

func TestFixedTTLExpiry(t *testing.T) {
	c := New(Config{TTL: time.Second, Eviction: LRU})
	base := time.Unix(1_700_000_000, 0)
	now := base
	c.now = func() time.Time { return now }

	req := makeReq(nameA)
	c.Set(req, makeResp(req))

	now = base.Add(500 * time.Millisecond)
	if c.Get(req) == nil {
		t.Fatal("未到固定 TTL，应命中")
	}

	now = base.Add(2 * time.Second)
	if c.Get(req) != nil {
		t.Fatal("超过固定 TTL，应过期")
	}
}

func TestRecordTTLFollow(t *testing.T) {
	// TTL<=0 时跟随记录自身 TTL。
	c := New(Config{TTL: 0, Eviction: LRU})
	req := makeReq(nameA)
	c.Set(req, makeResp(req)) // 记录 TTL=60s

	got := c.Get(req)
	if got == nil {
		t.Fatal("记录 TTL 未过期，应命中")
	}
	if tt := got.Answer[0].Header().Ttl; tt > 60 {
		t.Fatalf("剩余 TTL 不应超过记录 TTL，得到 %d", tt)
	}
}

func TestHitRewritesRemainingTTL(t *testing.T) {
	c := New(Config{TTL: 30 * time.Second, Eviction: LRU})
	base := time.Unix(1_700_000_000, 0)
	now := base
	c.now = func() time.Time { return now }

	req := makeReq(nameA)
	c.Set(req, makeResp(req))

	now = base.Add(10 * time.Second)
	got := c.Get(req)
	if got == nil {
		t.Fatal("应命中")
	}
	if tt := got.Answer[0].Header().Ttl; tt != 20 {
		t.Fatalf("剩余 TTL 应为 20，得到 %d", tt)
	}
}

func TestNonCacheable(t *testing.T) {
	c := New(Config{TTL: time.Minute, Eviction: LRU})
	req := makeReq(nameA)
	// 空应答：NOERROR 但 A 查询无 IP 亦无 SOA，不可缓存。
	c.Set(req, (&dns.Msg{}).SetReply(req))
	if c.Len() != 0 {
		t.Fatal("不可缓存的响应不应入库")
	}
}

func TestGetStale(t *testing.T) {
	c := New(Config{
		TTL:            30 * time.Second,
		Eviction:       LRU,
		StaleMaxAge:    time.Hour,
		StaleAnswerTTL: 5 * time.Second,
	})
	base := time.Unix(1_700_000_000, 0)
	now := base
	c.now = func() time.Time { return now }

	req := makeReq(nameA)
	c.Set(req, makeResp(req))

	// 未过期：GetStale 不返回，Get 正常命中。
	if _, ok := c.GetStale(req); ok {
		t.Fatal("未过期时 GetStale 不应命中 stale")
	}
	if c.Get(req) == nil {
		t.Fatal("未过期时 Get 应命中")
	}

	// 过期但在窗口内：Get 返回 nil（但不删），GetStale 命中并给短 TTL。
	now = base.Add(40 * time.Second)
	if c.Get(req) != nil {
		t.Fatal("已过期时 Get 应返回 nil")
	}
	if c.Len() != 1 {
		t.Fatal("窗口内的 stale 条目不应被删除")
	}
	resp, ok := c.GetStale(req)
	if !ok {
		t.Fatal("窗口内应命中 stale")
	}
	if tt := resp.Answer[0].Header().Ttl; tt != 5 {
		t.Fatalf("stale 响应 TTL 应为 5，得到 %d", tt)
	}

	// 超过窗口：彻底删除。
	now = base.Add(2 * time.Hour)
	if _, ok := c.GetStale(req); ok {
		t.Fatal("超过窗口后不应命中 stale")
	}
	if c.Len() != 0 {
		t.Fatal("超过窗口后条目应被删除")
	}
}

func TestGetStaleDisabled(t *testing.T) {
	c := New(Config{TTL: 30 * time.Second, Eviction: LRU, StaleMaxAge: 0})
	base := time.Unix(1_700_000_000, 0)
	now := base
	c.now = func() time.Time { return now }

	req := makeReq(nameA)
	c.Set(req, makeResp(req))

	now = base.Add(40 * time.Second)
	if _, ok := c.GetStale(req); ok {
		t.Fatal("未启用乐观缓存时不应命中 stale")
	}
	// 未启用 stale 时，过期条目的删除由 Get 负责。
	if c.Get(req) != nil {
		t.Fatal("已过期时 Get 应返回 nil")
	}
	if c.Len() != 0 {
		t.Fatal("未启用乐观缓存时过期条目应由 Get 删除")
	}
}

func TestClamp(t *testing.T) {
	c := New(Config{TTL: time.Minute, Eviction: LRU, MinTTL: 60 * time.Second, MaxTTL: 300 * time.Second})
	req := makeReq(nameA)
	// 构造 TTL 分别为 10（低于 min）、120（区间内）、600（高于 max）的三条记录。
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Answer = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 10}, A: net.IPv4(1, 2, 3, 4)},
		&dns.A{Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.IPv4(1, 2, 3, 5)},
		&dns.A{Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 600}, A: net.IPv4(1, 2, 3, 6)},
	}

	got := c.Clamp(resp)
	if got != resp {
		t.Fatal("Clamp 应原地修改并返回同一指针")
	}
	ttls := []uint32{got.Answer[0].Header().Ttl, got.Answer[1].Header().Ttl, got.Answer[2].Header().Ttl}
	want := []uint32{60, 120, 300}
	for i := range ttls {
		if ttls[i] != want[i] {
			t.Fatalf("记录 %d TTL 应为 %d，得到 %d", i, want[i], ttls[i])
		}
	}
}

func TestClampDisabled(t *testing.T) {
	c := New(Config{TTL: time.Minute, Eviction: LRU}) // MinTTL/MaxTTL 均为 0
	req := makeReq(nameA)
	resp := makeResp(req) // TTL=60

	got := c.Clamp(resp)
	if got != resp {
		t.Fatal("未启用钳制时 Clamp 应返回原指针")
	}
	if got.Answer[0].Header().Ttl != 60 {
		t.Fatalf("未启用钳制时 TTL 不应改变，得到 %d", got.Answer[0].Header().Ttl)
	}
}
