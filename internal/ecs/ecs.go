// Package ecs 实现 EDNS Client Subnet（ECS，RFC 7871）处理策略。
package ecs

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/miekg/dns"
)

// Policy 描述 ECS 处理策略。三种模式：
//   - off      关闭：移除发往上游请求中的整个 OPT（EDNS）记录，不传任何 EDNS 信息。
//   - pass     透传：保留客户端请求的 ECS 原样（依赖库的 EnableEDNSClientSubnet
//     实现透传 + 按 ECS 分键的 subnet 缓存）。
//   - override 覆写：移除客户端 ECS，强制写入配置的固定 ECS。
//
// 缓存正确性：
//   - off/override 模式下 ECS 是确定性的（无 / 固定），库的普通缓存 key
//     （不含 ECS）对所有客户端一致，不会串缓存。
//   - pass 模式下 ECS 随客户端变化，必须开启库的 EnableEDNSClientSubnet
//     才能走 subnet 缓存；该开关在 cmd/dnsproxy/main.go 按 mode 注入。
type Policy struct {
	mode   string       // "off" / "pass" / "override"
	prefix netip.Prefix // override 模式的固定前缀（已规范化）
}

// New 根据模式与地址构造 ECS 策略，并校验 override 模式的前缀合法。
func New(mode, address string) (*Policy, error) {
	switch mode {
	case "off", "pass":
		return &Policy{mode: mode}, nil
	case "override":
		p, err := netip.ParsePrefix(address)
		if err != nil {
			return nil, fmt.Errorf("ecs.address 非法 %q: %w", address, err)
		}
		return &Policy{mode: mode, prefix: p.Masked()}, nil
	default:
		return nil, fmt.Errorf("ecs.mode 必须为 off/pass/override，当前为 %q", mode)
	}
}

// Apply 按策略修改发往上游的请求。pass 模式不在此处处理（交由库透传）。
func (e *Policy) Apply(m *dns.Msg) {
	switch e.mode {
	case "off":
		stripOPT(m)
	case "override":
		setECSOverride(m, e.prefix)
	}
}

// stripOPT 从消息中移除所有 OPT（EDNS0）记录。
func stripOPT(m *dns.Msg) {
	if len(m.Extra) == 0 {
		return
	}
	extra := m.Extra[:0]
	for _, rr := range m.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		extra = append(extra, rr)
	}
	m.Extra = extra
}

// setECSOverride 移除消息中已有的 ECS 选项，并写入固定的 ECS 前缀。
// 保留 OPT 中的其他选项（DO bit、COOKIE 等）。
func setECSOverride(m *dns.Msg, prefix netip.Prefix) {
	opt := m.IsEdns0()
	if opt == nil {
		opt = &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
		m.Extra = append(m.Extra, opt)
	}

	// 移除现有 ECS 选项。
	opts := opt.Option[:0]
	for _, o := range opt.Option {
		if _, ok := o.(*dns.EDNS0_SUBNET); ok {
			continue
		}
		opts = append(opts, o)
	}
	opt.Option = opts

	// 构造固定 ECS：地址已按前缀截断，SourceScope 置 0（RFC 7871 第 6 节）。
	addr := prefix.Addr()
	e := &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Address:       net.IP(addr.AsSlice()),
		SourceNetmask: uint8(prefix.Bits()),
		SourceScope:   0,
	}
	if addr.Is4() {
		e.Family = 1
	} else {
		e.Family = 2
	}

	opt.Option = append(opt.Option, e)
}
