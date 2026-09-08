package handler

import (
	"testing"

	"github.com/miekg/dns"
)

func newMsg() *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	return m
}

func TestSetDOAddsOptWhenAbsent(t *testing.T) {
	m := newMsg()
	setDO(m)

	opt := m.IsEdns0()
	if opt == nil {
		t.Fatal("setDO 应新建 OPT 记录")
	}
	if !opt.Do() {
		t.Fatal("setDO 应设置 DO 位")
	}
}

func TestSetDOOnExistingOpt(t *testing.T) {
	m := newMsg()
	// 已有 OPT 但 DO 未置位。
	m.SetEdns0(4096, false)
	setDO(m)

	opt := m.IsEdns0()
	if opt == nil || !opt.Do() {
		t.Fatal("已有 OPT 时 setDO 应保留并置 DO 位")
	}
	if opt.UDPSize() != 4096 {
		t.Fatalf("已有 OPT 的 UDP size 不应被改写，得到 %d", opt.UDPSize())
	}
}

func TestSetDOPreservesDoWhenSet(t *testing.T) {
	m := newMsg()
	m.SetEdns0(1232, true)
	setDO(m)

	if opt := m.IsEdns0(); opt == nil || !opt.Do() {
		t.Fatal("DO 已置位时 setDO 应保持")
	}
}

func TestClientWantsDO(t *testing.T) {
	// 无 OPT。
	if clientWantsDO(newMsg()) {
		t.Fatal("无 OPT 时 clientWantsDO 应为 false")
	}
	// 有 OPT 但 DO 未置位。
	m := newMsg()
	m.SetEdns0(4096, false)
	if clientWantsDO(m) {
		t.Fatal("DO 未置位时 clientWantsDO 应为 false")
	}
	// DO 已置位。
	m2 := newMsg()
	m2.SetEdns0(4096, true)
	if !clientWantsDO(m2) {
		t.Fatal("DO 置位时 clientWantsDO 应为 true")
	}
}
