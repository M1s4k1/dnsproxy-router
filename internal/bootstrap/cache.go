// Package bootstrap 提供引导 DNS 解析结果的固定 TTL 缓存。
package bootstrap

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// entry 是一次解析结果及其过期时间。
type entry struct {
	addrs  []netip.Addr
	expire time.Time
}

// Cache 包装一个底层 Resolver，为解析结果提供固定 TTL 缓存。
//
// 与库自带的 CachingResolver 不同：后者缓存 TTL 跟随 DNS 记录自身 TTL，
// 而这里使用用户配置的固定时长，便于在「解析结果短期稳定」的场景下
// 主动控制缓存窗口。
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	inner upstream.Resolver
	cache map[string]entry
}

// New 构造一个固定 TTL 的 bootstrap 解析缓存。ttl 需为正数。
func New(inner upstream.Resolver, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &Cache{
		ttl:   ttl,
		inner: inner,
		cache: make(map[string]entry),
	}
}

// LookupNetIP 实现 upstream.Resolver 接口：缓存命中且未过期则直接返回，
// 否则向下游解析并写入缓存。
func (c *Cache) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	key := network + "\x00" + host
	now := time.Now()

	c.mu.Lock()
	e, ok := c.cache[key]
	c.mu.Unlock()
	if ok && now.Before(e.expire) {
		return slices.Clone(e.addrs), nil
	}

	addrs, err := c.inner.LookupNetIP(ctx, network, host)
	if err != nil {
		return addrs, err
	}

	c.mu.Lock()
	c.cache[key] = entry{addrs: addrs, expire: now.Add(c.ttl)}
	c.mu.Unlock()

	return slices.Clone(addrs), nil
}

var _ upstream.Resolver = (*Cache)(nil)
