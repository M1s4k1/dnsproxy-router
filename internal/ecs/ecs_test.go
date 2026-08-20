package ecs

import (
	"net"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
)

// 测试 setECSOverride 是否正确覆写 ECS
func TestSetECSOverride(t *testing.T) {
	// 构造一个带 DO bit + COOKIE 的请求，验证覆写保留其他选项
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	m.SetEdns0(4096, true) // DO bit
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{
		Code:   dns.EDNS0COOKIE,
		Cookie: "abcdef0123456789",
	})

	pfx := netip.MustParsePrefix("223.5.5.0/24")
	setECSOverride(m, pfx)

	opt = m.IsEdns0()
	if opt == nil {
		t.Fatal("覆写后 OPT 不应为 nil")
	}
	// 应有 ECS + COOKIE 两个选项
	if len(opt.Option) != 2 {
		t.Fatalf("期望 2 个选项（ECS+COOKIE），得到 %d", len(opt.Option))
	}
	var ecs *dns.EDNS0_SUBNET
	for _, o := range opt.Option {
		if e, ok := o.(*dns.EDNS0_SUBNET); ok {
			ecs = e
		}
	}
	if ecs == nil {
		t.Fatal("未找到 ECS 选项")
	}
	if ecs.Family != 1 || ecs.SourceNetmask != 24 || ecs.SourceScope != 0 {
		t.Fatalf("ECS 字段错误: family=%d netmask=%d scope=%d", ecs.Family, ecs.SourceNetmask, ecs.SourceScope)
	}
	if !net.IP(ecs.Address).Equal(net.ParseIP("223.5.5.0")) {
		t.Fatalf("ECS 地址错误: %s", ecs.Address)
	}
	if !opt.Do() {
		t.Fatal("DO bit 应被保留")
	}
}

// 测试 stripOPT 移除所有 OPT
func TestStripOPT(t *testing.T) {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	m.SetEdns0(4096, false)
	if m.IsEdns0() == nil {
		t.Fatal("setup 失败：应有 OPT")
	}
	stripOPT(m)
	if m.IsEdns0() != nil {
		t.Fatal("stripOPT 后 OPT 应被移除")
	}
	if len(m.Extra) != 0 {
		t.Fatalf("stripOPT 后 Extra 应为空，得到 %d", len(m.Extra))
	}
}

// 测试 override 重复调用幂等（不会累积多个 ECS）
func TestSetECSOverrideIdempotent(t *testing.T) {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	pfx := netip.MustParsePrefix("8.8.8.0/24")
	for i := 0; i < 3; i++ {
		setECSOverride(m, pfx)
	}
	opt := m.IsEdns0()
	cnt := 0
	for _, o := range opt.Option {
		if _, ok := o.(*dns.EDNS0_SUBNET); ok {
			cnt++
		}
	}
	if cnt != 1 {
		t.Fatalf("ECS 选项应只有 1 个，得到 %d", cnt)
	}
}

// 测试 IPv6 前缀
func TestSetECSOverrideIPv6(t *testing.T) {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	pfx := netip.MustParsePrefix("2001:db8::/32")
	setECSOverride(m, pfx)
	opt := m.IsEdns0()
	var ecs *dns.EDNS0_SUBNET
	for _, o := range opt.Option {
		if e, ok := o.(*dns.EDNS0_SUBNET); ok {
			ecs = e
		}
	}
	if ecs == nil || ecs.Family != 2 || ecs.SourceNetmask != 32 {
		t.Fatalf("IPv6 ECS 错误: %+v", ecs)
	}
}
