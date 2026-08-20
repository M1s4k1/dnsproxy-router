package afp

import (
	"context"
	"net/netip"
	"slices"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// Selector 包装底层 resolver，按 Prober 的优选族过滤返回地址：
// 若底层返回了优选族地址，则只返回该族；否则原样返回（退化到单栈）。
type Selector struct {
	inner  upstream.Resolver
	prober *Prober
	prefer Family // 非 latency 模式下的静态偏好（无需 prober）
}

// NewSelector 构造按优选族过滤的 resolver。prefer 为静态偏好（ipv4/ipv6），
// prober 仅在「按延迟」模式下非 nil 并动态决定优选族。
func NewSelector(inner upstream.Resolver, prefer Family, prober *Prober) *Selector {
	return &Selector{inner: inner, prefer: prefer, prober: prober}
}

// LookupNetIP 实现 upstream.Resolver：先解析，再按优选族过滤。
func (s *Selector) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	addrs, err := s.inner.LookupNetIP(ctx, network, host)
	if err != nil {
		return addrs, err
	}

	// ip4/ip6 网络由底层（HostsResolver 等）已按族过滤，无需二次过滤。
	if network == "ip4" || network == "ip6" {
		return addrs, nil
	}

	prefer := s.prefer
	if s.prober != nil {
		prefer = s.prober.Current()
	}

	return preferFamily(addrs, prefer), nil
}

// preferFamily 只保留 prefer 族的地址；若无该族地址则原样返回全部。
func preferFamily(addrs []netip.Addr, prefer Family) []netip.Addr {
	var keep []netip.Addr
	for _, a := range addrs {
		if FamilyOf(a) == prefer {
			keep = append(keep, a)
		}
	}
	if len(keep) == 0 {
		return addrs
	}
	return slices.Clone(keep)
}

var _ upstream.Resolver = (*Selector)(nil)
