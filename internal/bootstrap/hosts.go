package bootstrap

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// HostsResolver 是「主机名 → IP」的静态映射 resolver：命中即返回给定 IP，
// 不再查询底层引导 DNS；未命中则回退到底层 resolver。
//
// 与库自带的 HostsResolver（读系统 hosts 文件）不同，这里的数据来自用户配置。
// 地址族的优先级（IPv4/IPv6 谁先）由库在拨号时按 PreferIPv6 排序并逐个尝试，
// 故此处对 "ip" 网络原样返回全部地址，交由库做连接级回退。
type HostsResolver struct {
	hosts map[string][]netip.Addr
	inner upstream.Resolver
}

// NewHostsResolver 构造一个静态映射 resolver。hosts 的键为主机名，值为 IP 列表。
// 键在构造时统一小写并去掉尾部点，与上游地址里的 hostname 对齐。
func NewHostsResolver(hosts map[string][]netip.Addr, inner upstream.Resolver) *HostsResolver {
	norm := make(map[string][]netip.Addr, len(hosts))
	for name, addrs := range hosts {
		norm[normalizeHost(name)] = addrs
	}
	return &HostsResolver{hosts: norm, inner: inner}
}

// normalizeHost 将主机名小写并去掉尾部点，作为映射键的规范形式。
func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// LookupNetIP 实现 upstream.Resolver 接口：先查静态映射（按 network 过滤地址族），
// 命中且该网络有匹配地址则返回；否则回退底层 resolver。
func (r *HostsResolver) LookupNetIP(
	ctx context.Context,
	network string,
	host string,
) (addrs []netip.Addr, err error) {
	if ips, ok := r.hosts[normalizeHost(host)]; ok {
		addrs = filterByNetwork(ips, network)
		if len(addrs) > 0 {
			return slices.Clone(addrs), nil
		}
	}
	return r.inner.LookupNetIP(ctx, network, host)
}

// filterByNetwork 按 network 过滤地址：ip4 只留 IPv4，ip6 只留 IPv6，ip 全留。
func filterByNetwork(ips []netip.Addr, network string) []netip.Addr {
	switch network {
	case "ip4":
		out := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			if ip.Is4() {
				out = append(out, ip)
			}
		}
		return out
	case "ip6":
		out := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			if ip.Is6() {
				out = append(out, ip)
			}
		}
		return out
	default:
		return ips
	}
}

var _ upstream.Resolver = (*HostsResolver)(nil)
