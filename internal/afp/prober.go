// Package afp 实现「地址族优先级」（Address-Family Preference）的动态选族。
//
// 当上游域名同时有 IPv4/IPv6 时，支持按延迟自动选择低延迟的地址族：
// Prober 周期性解析每个上游域名、分别测两族的连接延迟，比较全局平均延迟
// 后把优选族写入一个原子开关；Selector 则包装引导解析链，按该开关只返回
// 优选族的地址。
package afp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
)

// Family 是地址族。其 bool 值即为原子开关的取值：false=IPv4，true=IPv6。
type Family bool

const (
	// IPv4 表示优先 IPv4。
	IPv4 Family = false
	// IPv6 表示优先 IPv6。
	IPv6 Family = true
)

// FamilyOf 返回 addr 所属地址族。
func FamilyOf(addr netip.Addr) Family {
	return Family(addr.Is6())
}

// Target 是一个探测目标：上游主机名 + 端口。
type Target struct {
	Host string
	Port uint16
}

// Prober 周期探测各上游域名的地址族延迟，维护「当前优选族」。
type Prober struct {
	logger   *slog.Logger
	resolver upstream.Resolver
	interval time.Duration
	now      func() time.Time

	current atomic.Bool // false=IPv4，true=IPv6
	targets []Target
}

// NewProber 构造探测器。targets 为探测目标（主机名+端口）。
func NewProber(logger *slog.Logger, resolver upstream.Resolver, targets []Target, interval time.Duration) *Prober {
	return &Prober{
		logger:   logger,
		resolver: resolver,
		interval: interval,
		now:      time.Now,
		targets:  targets,
	}
}

// Current 返回当前优选族。
func (p *Prober) Current() Family { return Family(p.current.Load()) }

// Start 启动探测循环：立即探测一轮，之后每 interval 重测，退出时关闭。
func (p *Prober) Start(ctx context.Context) {
	p.probe(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probe(ctx)
		}
	}
}

// probe 解析每个域名、分别测两族延迟，选全局平均延迟更低的族。
func (p *Prober) probe(ctx context.Context) {
	var (
		mu      sync.Mutex
		v4Count int
		v6Count int
		v4Total time.Duration
		v6Total time.Duration
	)

	var wg sync.WaitGroup
	for _, t := range p.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			addrs, err := p.resolver.LookupNetIP(ctx, "ip", t.Host)
			if err != nil || len(addrs) == 0 {
				return
			}
			for _, a := range addrs {
				rtt, ok := p.measure(a, t.Port)
				if !ok {
					continue
				}
				mu.Lock()
				if a.Is6() {
					v6Count++
					v6Total += rtt
				} else {
					v4Count++
					v4Total += rtt
				}
				mu.Unlock()
			}
		}(t)
	}
	wg.Wait()

	if v4Count == 0 && v6Count == 0 {
		p.logger.Warn("双栈延迟探测：无可测地址，维持当前优选族", "current", p.Current())
		return
	}

	prefer := IPv6
	switch {
	case v6Count == 0:
		prefer = IPv4
	case v4Count == 0:
		prefer = IPv6
	default:
		if float64(v4Total)/float64(v4Count) <= float64(v6Total)/float64(v6Count) {
			prefer = IPv4
		}
	}

	p.current.Store(bool(prefer))
	p.logger.Info("双栈延迟探测完成",
		"prefer", prefer,
		"v4_count", v4Count,
		"v6_count", v6Count,
		"v4_avg_ms", durMs(v4Total, v4Count),
		"v6_avg_ms", durMs(v6Total, v6Count),
	)
}

// measure 用 TCP 连接测单个 IP:port 的延迟；失败返回 ok=false。
func (p *Prober) measure(addr netip.Addr, port uint16) (time.Duration, bool) {
	start := p.now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr.String(), strconv.FormatUint(uint64(port), 10)), 2*time.Second)
	if err != nil {
		return 0, false
	}
	_ = conn.Close()
	return p.now().Sub(start), true
}

func durMs(total time.Duration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(total.Milliseconds()) / float64(n)
}
