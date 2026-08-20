package bootstrap

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// countingResolver 是一个记录调用次数的 mock resolver，始终返回固定地址。
type countingResolver struct {
	calls atomic.Int32
}

func (r *countingResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	return []netip.Addr{netip.MustParseAddr("1.2.3.4")}, nil
}

// TestCacheHit 验证缓存未过期时命中，不重复调用底层 resolver。
func TestCacheHit(t *testing.T) {
	inner := &countingResolver{}
	c := New(inner, time.Minute)

	got, err := c.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 1 {
		t.Fatalf("首次查询失败: got=%v err=%v", got, err)
	}
	got2, err := c.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got2) != 1 {
		t.Fatalf("二次查询失败: got=%v err=%v", got2, err)
	}

	if inner.calls.Load() != 1 {
		t.Fatalf("缓存未命中：底层 resolver 被调用了 %d 次，期望 1 次", inner.calls.Load())
	}
}

// TestCacheExpire 验证缓存过期后重新解析。
func TestCacheExpire(t *testing.T) {
	inner := &countingResolver{}
	c := New(inner, 10*time.Millisecond)

	if _, err := c.LookupNetIP(context.Background(), "ip", "example.com."); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // 等缓存过期
	if _, err := c.LookupNetIP(context.Background(), "ip", "example.com."); err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}

	if inner.calls.Load() != 2 {
		t.Fatalf("缓存应已过期：底层 resolver 被调用了 %d 次，期望 2 次", inner.calls.Load())
	}
}

// TestCacheKeySeparatesNetwork 验证不同 network 使用独立缓存键。
func TestCacheKeySeparatesNetwork(t *testing.T) {
	inner := &countingResolver{}
	c := New(inner, time.Minute)

	if _, err := c.LookupNetIP(context.Background(), "ip", "example.com."); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LookupNetIP(context.Background(), "ip4", "example.com."); err != nil {
		t.Fatal(err)
	}

	if inner.calls.Load() != 2 {
		t.Fatalf("不同 network 应独立缓存：底层 resolver 被调用了 %d 次，期望 2 次", inner.calls.Load())
	}
}
