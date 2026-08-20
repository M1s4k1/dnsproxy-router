package afp

import (
	"context"
	"net/netip"
	"testing"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// fakeResolver 返回固定地址列表。
type fakeResolver []netip.Addr

func (f fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return []netip.Addr(f), nil
}

func dualStack() []netip.Addr {
	return []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),
		netip.MustParseAddr("2001:db8::1"),
	}
}

func onlyV4() []netip.Addr {
	return []netip.Addr{netip.MustParseAddr("1.2.3.4")}
}

// TestSelectorPreferV4 验证 ipv4 优先时只返回 IPv4。
func TestSelectorPreferV4(t *testing.T) {
	s := NewSelector(fakeResolver(dualStack()), IPv4, nil)
	got, err := s.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 1 || got[0].Is6() {
		t.Fatalf("应只返回 IPv4: got=%v err=%v", got, err)
	}
}

// TestSelectorPreferV6 验证 ipv6 优先时只返回 IPv6。
func TestSelectorPreferV6(t *testing.T) {
	s := NewSelector(fakeResolver(dualStack()), IPv6, nil)
	got, err := s.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 1 || got[0].Is4() {
		t.Fatalf("应只返回 IPv6: got=%v err=%v", got, err)
	}
}

// TestSelectorFallbackSingleStack 验证底层只有单栈时原样返回。
func TestSelectorFallbackSingleStack(t *testing.T) {
	s := NewSelector(fakeResolver(onlyV4()), IPv6, nil)
	got, err := s.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 1 || got[0].Is6() {
		t.Fatalf("单栈应原样返回: got=%v err=%v", got, err)
	}
}

// TestSelectorDynamic 验证 prober 动态决定优选族。
func TestSelectorDynamic(t *testing.T) {
	p := &Prober{}
	p.current.Store(bool(IPv6))

	s := NewSelector(fakeResolver(dualStack()), IPv4, p)
	got, err := s.LookupNetIP(context.Background(), "ip", "example.com.")
	if err != nil || len(got) != 1 || got[0].Is4() {
		t.Fatalf("prober 选 IPv6 时应只返回 IPv6: got=%v err=%v", got, err)
	}
}

// TestSelectorPassthroughNetwork 验证 ip4/ip6 网络不做二次过滤。
func TestSelectorPassthroughNetwork(t *testing.T) {
	s := NewSelector(fakeResolver(dualStack()), IPv4, nil)
	got, err := s.LookupNetIP(context.Background(), "ip6", "example.com.")
	if err != nil || len(got) != 2 {
		t.Fatalf("ip6 网络应由底层过滤，此处应透传: got=%v err=%v", got, err)
	}
}

var _ upstream.Resolver = (*Selector)(nil)
