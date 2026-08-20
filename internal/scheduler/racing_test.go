package scheduler

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeUpstream 返回可配置延迟的固定响应，用于测试赛马选择逻辑。
type fakeUpstream struct {
	addr    string
	delay   time.Duration
	id      string // 写入响应的 Extra，便于断言选中的是哪家
	exchang atomic.Int64
}

func (f *fakeUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
	f.exchang.Add(1)
	time.Sleep(f.delay)
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Extra = append(resp.Extra, &dns.OPT{
		Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT},
	})
	// 用 TXT 记录承载 id 供断言。
	resp.Answer = append(resp.Answer, &dns.TXT{
		Hdr: dns.RR_Header{Name: "id.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
		Txt: []string{f.id},
	})
	return resp, nil
}

func (f *fakeUpstream) Address() string { return f.addr }

func (f *fakeUpstream) Close() error { return nil }

func question() *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	return m
}

func member(name string, weight int, delay time.Duration) racingMember {
	return racingMember{
		name:     name,
		weight:   weight,
		upstream: &fakeUpstream{addr: name, delay: delay, id: name},
	}
}

func chosenID(t *testing.T, r *racingUpstream) string {
	t.Helper()
	resp, err := r.Exchange(question())
	if err != nil {
		t.Fatalf("Exchange 失败: %v", err)
	}
	for _, rr := range resp.Answer {
		if txt, ok := rr.(*dns.TXT); ok {
			return txt.Txt[0]
		}
	}
	t.Fatalf("响应未携带 id 标记")
	return ""
}

// TestFastestPicksFirst 验证 fastest 模式选最快返回者。
func TestFastestPicksFirst(t *testing.T) {
	r := newRacing([]racingMember{
		member("slow", 1, 100*time.Millisecond),
		member("fast", 1, 5*time.Millisecond),
		member("mid", 1, 30*time.Millisecond),
	}, false, 0)

	if got := chosenID(t, r); got != "fast" {
		t.Fatalf("fastest 应选 fast，得到 %q", got)
	}
}

// TestWeightedPicksHighestInWindow 验证加权模式：窗口内选权重最高者。
func TestWeightedPicksHighestInWindow(t *testing.T) {
	r := newRacing([]racingMember{
		member("first", 1, 5*time.Millisecond),   // 首个返回，权重 1
		member("heavy", 20, 30*time.Millisecond), // 窗口内返回，权重 20
		member("mid", 8, 20*time.Millisecond),    // 窗口内返回，权重 8
	}, true, 50*time.Millisecond)

	if got := chosenID(t, r); got != "heavy" {
		t.Fatalf("加权模式应选 heavy（权重最高），得到 %q", got)
	}
}

// TestWeightedTimeoutDropped 验证加权模式：窗口外的响应被抛弃。
func TestWeightedTimeoutDropped(t *testing.T) {
	r := newRacing([]racingMember{
		member("first", 1, 5*time.Millisecond),         // 首个返回，权重 1，开启窗口
		member("late-heavy", 20, 200*time.Millisecond), // 窗口外返回，虽权重高但被抛弃
	}, true, 30*time.Millisecond)

	if got := chosenID(t, r); got != "first" {
		t.Fatalf("窗口外的高权重响应应被抛弃，得到 %q", got)
	}
}

// TestWeightedTiePicksFastest 验证加权模式：权重相同时选最快者。
func TestWeightedTiePicksFastest(t *testing.T) {
	r := newRacing([]racingMember{
		member("a", 5, 40*time.Millisecond),
		member("b", 5, 10*time.Millisecond),
		member("c", 5, 25*time.Millisecond),
	}, true, 100*time.Millisecond)

	if got := chosenID(t, r); got != "b" {
		t.Fatalf("权重相同时应选最快 b，得到 %q", got)
	}
}

// TestSingleMember 验证单成员直接透传。
func TestSingleMember(t *testing.T) {
	r := newRacing([]racingMember{member("only", 7, 1*time.Millisecond)}, true, 50*time.Millisecond)
	if got := chosenID(t, r); got != "only" {
		t.Fatalf("单成员应透传 only，得到 %q", got)
	}
}
