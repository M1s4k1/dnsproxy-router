package scheduler

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeUpstream 返回可配置延迟的固定响应，用于测试赛马选择逻辑。
type fakeUpstream struct {
	addr  string
	delay time.Duration
	id    string // 写入响应，便于断言选中的是哪家
}

func (f *fakeUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
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

// countingUpstream 统计 Exchange 调用次数，可配置返回错误，用于验证熔断剔除。
type countingUpstream struct {
	id    string
	fail  bool
	delay time.Duration
	calls atomic.Int32
}

func (c *countingUpstream) Exchange(req *dns.Msg) (*dns.Msg, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.fail {
		return nil, errors.New("模拟上游失败")
	}
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Answer = append(resp.Answer, &dns.TXT{
		Hdr: dns.RR_Header{Name: "id.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
		Txt: []string{c.id},
	})
	return resp, nil
}

func (c *countingUpstream) Address() string { return c.id }
func (c *countingUpstream) Close() error    { return nil }

// TestBreakerSkipsOpenedMember 验证熔断生效：某成员连续失败达阈值后不再被调用，
// 健康成员仍正常参与。
func TestBreakerSkipsOpenedMember(t *testing.T) {
	bad := &countingUpstream{id: "bad", fail: true}
	good := &countingUpstream{id: "good", delay: 20 * time.Millisecond} // 让 bad 的失败路径每次都在 good 返回前跑完

	r := newRacing([]racingMember{
		{weight: 1, upstream: bad},
		{weight: 1, upstream: good},
	}, false, 0)
	r.breaker = newBreaker(2, 2, time.Hour, nil) // 阈值 2，冷却 1h（测试期间不会恢复）

	// 前两次：bad 各失败一次（第 2 次失败后 report 熔断）。good 均成功，返回 good。
	for i := 0; i < 2; i++ {
		if got := chosenID(t, r); got != "good" {
			t.Fatalf("第 %d 次应选 good，得到 %q", i+1, got)
		}
	}
	if bad.calls.Load() != 2 {
		t.Fatalf("前两次 bad 应被调用 2 次，实际 %d", bad.calls.Load())
	}

	// 第三次：bad 已熔断被剔除，不再调用；仅 good 参与。
	if got := chosenID(t, r); got != "good" {
		t.Fatalf("熔断后应选 good，得到 %q", got)
	}
	if bad.calls.Load() != 2 {
		t.Fatalf("熔断后 bad 不应再被调用，实际 %d", bad.calls.Load())
	}
}

// TestSingleMember 验证单成员直接透传。
func TestSingleMember(t *testing.T) {
	r := newRacing([]racingMember{member("only", 7, 1*time.Millisecond)}, true, 50*time.Millisecond)
	if got := chosenID(t, r); got != "only" {
		t.Fatalf("单成员应透传 only，得到 %q", got)
	}
}

// TestWeightedReturnsAtWindowEnd 验证窗口结束后立即返回，不等慢上游。
func TestWeightedReturnsAtWindowEnd(t *testing.T) {
	r := newRacing([]racingMember{
		member("first", 1, 5*time.Millisecond),   // 首个成功，开启窗口
		member("slow", 20, 500*time.Millisecond), // 窗口外才返回（权重最高）
	}, true, 30*time.Millisecond)

	start := time.Now()
	if got := chosenID(t, r); got != "first" {
		t.Fatalf("窗口内应选 first，得到 %q", got)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("窗口结束应即返回，不应等慢上游；耗时 %v", elapsed)
	}
}
