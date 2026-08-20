package bootstrap

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// countingResolver 记录调用次数，返回固定 IPv4 地址。
type hostsCountingResolver struct {
	calls atomic.Int32
}

func (r *hostsCountingResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	return []netip.Addr{netip.MustParseAddr("9.9.9.9")}, nil
}

func testHosts() map[string][]netip.Addr {
	return map[string][]netip.Addr{
		"example.com": {
			netip.MustParseAddr("1.2.3.4"),
			netip.MustParseAddr("2001:db8::1"),
		},
		"v6only.com": {netip.MustParseAddr("2001:db8::2")},
	}
}

// TestHostsHit 验证命中静态映射时不调用底层 resolver。
func TestHostsHit(t *testing.T) {
	inner := &hostsCountingResolver{}
	r := NewHostsResolver(testHosts(), inner)

	got, err := r.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 2 {
		t.Fatalf("命中映射查询失败: got=%v err=%v", got, err)
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("命中映射不应调用底层 resolver，实际调用了 %d 次", inner.calls.Load())
	}
}

// TestHostsMissFallback 验证未命中时回退底层 resolver。
func TestHostsMissFallback(t *testing.T) {
	inner := &hostsCountingResolver{}
	r := NewHostsResolver(testHosts(), inner)

	got, err := r.LookupNetIP(context.Background(), "ip", "unknown.com.")
	if err != nil || len(got) != 1 {
		t.Fatalf("未命中回退查询失败: got=%v err=%v", got, err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("未命中应回退底层 resolver，实际调用了 %d 次", inner.calls.Load())
	}
}

// TestHostsFilterByNetwork 验证按 network 过滤地址族。
func TestHostsFilterByNetwork(t *testing.T) {
	inner := &hostsCountingResolver{}
	r := NewHostsResolver(testHosts(), inner)

	got4, err := r.LookupNetIP(context.Background(), "ip4", "example.com.")
	if err != nil || len(got4) != 1 || !got4[0].Is4() {
		t.Fatalf("ip4 过滤错误: got=%v err=%v", got4, err)
	}
	got6, err := r.LookupNetIP(context.Background(), "ip6", "example.com.")
	if err != nil || len(got6) != 1 || !got6[0].Is6() {
		t.Fatalf("ip6 过滤错误: got=%v err=%v", got6, err)
	}
}

// TestHostsFallbackWhenFamilyAbsent 验证某地址族未映射时回退底层。
func TestHostsFallbackWhenFamilyAbsent(t *testing.T) {
	inner := &hostsCountingResolver{}
	r := NewHostsResolver(testHosts(), inner)

	// v6only.com 只有 IPv6，查 ip4 应回退底层。
	_, err := r.LookupNetIP(context.Background(), "ip4", "v6only.com.")
	if err != nil {
		t.Fatalf("回退底层失败: %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("该地址族未映射应回退底层，实际调用了 %d 次", inner.calls.Load())
	}
}

var _ upstream.Resolver = (*HostsResolver)(nil)
