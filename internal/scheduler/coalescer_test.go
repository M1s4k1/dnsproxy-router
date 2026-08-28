package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// TestCoalescerSharesResult 验证并发相同 key 只执行一次 fn，且所有调用者拿到
// 语义一致的结果；同时用 -race 检测响应指针是否被并发读写（等待者读快照）。
// fn 用 release 屏障阻塞，确保所有 goroutine 与首个调用真正重叠，避免 fn
// 过快导致后续调用落进「已完成」窗口而各自重新执行（那本是正确语义）。
func TestCoalescerSharesResult(t *testing.T) {
	g := &coalescer{}
	req := question()

	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0

	const n = 50
	var wg sync.WaitGroup
	resps := make([]*dns.Msg, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := g.Do("example.com.", func() (*dns.Msg, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release // 阻塞，直到主 goroutine 释放，让其余请求重叠等待
				r := new(dns.Msg)
				r.SetReply(req)
				r.Answer = append(r.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: "id.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
					Txt: []string{"shared"},
				})
				return r, nil
			})
			resps[i] = resp
			errs[i] = err
		}(i)
	}

	// 留出时间让全部 goroutine 进入 Do（首个阻塞在 release，其余阻塞在 Wait）。
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("fn 应只执行一次，实际 %d 次", got)
	}

	close(release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("调用 %d 出错: %v", i, errs[i])
		}
		if resps[i] == nil {
			t.Fatalf("调用 %d 返回 nil 响应", i)
		}
	}

	// 并发改写各响应副本：若等待者共享了发起者的原始底层指针，-race 会在此
	// 暴露（发起者返回的原始 resp 由上层改写，等待者应持有独立快照）。
	for i := 0; i < n; i++ {
		for _, rr := range resps[i].Answer {
			rr.Header().Ttl = uint32(i + 1)
		}
	}
}

// TestCoalescerPropagatesError 验证 fn 出错时，等待者同样收到该错误。
func TestCoalescerPropagatesError(t *testing.T) {
	g := &coalescer{}
	release := make(chan struct{})
	sentinel := &dns.Error{}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = g.Do("example.com.", func() (*dns.Msg, error) {
				<-release
				return nil, sentinel
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != sentinel {
			t.Fatalf("调用 %d 应收到 sentinel 错误，得到 %v", i, err)
		}
	}
}
