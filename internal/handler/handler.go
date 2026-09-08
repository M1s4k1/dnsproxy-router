// Package handler 实现 proxy.Handler，把调度器的选路注入每个请求。
package handler

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/miekg/dns"

	"dnsproxy-router/internal/ecs"
	"dnsproxy-router/internal/scheduler"
)

// Handler 把 scheduler 当前周期的选路（各家最优线路 + 缓存）注入每个请求，
// 再交还 p.Resolve：既保留库的缓存、去重、SERVFAIL 兜底，
// 又实现「多家并发赛马」（依赖 proxy.UpstreamModeParallel）。
type Handler struct {
	// sched 用原子指针保存当前调度器，支持 SIGHUP 热重载时无锁热切换。
	sched          atomic.Pointer[scheduler.Scheduler]
	logger         *slog.Logger
	ecs            *ecs.Policy
	dnssec         bool
	fallbackLogged atomic.Bool
}

func New(sched *scheduler.Scheduler, logger *slog.Logger, ecs *ecs.Policy, dnssec bool) *Handler {
	h := &Handler{logger: logger, ecs: ecs, dnssec: dnssec}
	h.sched.Store(sched)
	return h
}

// Swap 热切换到新的调度器，返回旧调度器供调用方优雅退役。
func (h *Handler) Swap(s *scheduler.Scheduler) *scheduler.Scheduler {
	return h.sched.Swap(s)
}

// Current 返回当前生效的调度器（可能为 nil，若尚未初始化）。
func (h *Handler) Current() *scheduler.Scheduler {
	return h.sched.Load()
}

// Ready 报告选路是否已就绪（首轮探测完成且至少一条线路可用）。
func (h *Handler) Ready() bool {
	s := h.sched.Load()
	return s != nil && s.CurrentConfig() != nil
}

func (h *Handler) logOnce(msg string) {
	if h.fallbackLogged.CompareAndSwap(false, true) {
		h.logger.Warn(msg)
	}
}

func (h *Handler) ServeDNS(ctx context.Context, p *proxy.Proxy, dctx *proxy.DNSContext) error {
	s := h.sched.Load()
	var cfg *proxy.CustomUpstreamConfig
	if s != nil {
		cfg = s.CurrentConfig()
	}
	if cfg == nil {
		// 选路尚未就绪（首轮探测未完成，或所有上游均不可用）。
		// 仅在选路为 nil 的整段期间提示一次，避免每个请求刷屏。
		h.logOnce("选路尚未就绪，回退默认上游")
		return proxy.DefaultHandler{}.ServeDNS(ctx, p, dctx)
	}

	h.fallbackLogged.Store(false)

	// 客户端是否请求 DNSSEC 记录（DO 位）。必须在 ECS 处理前读取：ECS off 会
	// 剥掉整个 OPT（含 DO），处理后再读无法判断客户端原始意图。
	clientDO := h.dnssec && clientWantsDO(dctx.Req)

	// 按 ECS 策略修改发往上游的请求（pass 模式由库透传，无需在此处理）。
	h.ecs.Apply(dctx.Req)

	// DNSSEC 透传：仅当客户端请求带 DO 位时，才向上游设 DO 位让上游返回并
	// 缓存 RRSIG。必须放在 ECS 处理之后——ECS off 会剥掉整个 OPT（含 DO），
	// 若先设 DO 会被移除。无条件设 DO 会让未带 DO 的客户端也收到签名记录。
	if clientDO {
		setDO(dctx.Req)
	}

	dctx.CustomUpstreamConfig = cfg
	return p.Resolve(ctx, dctx)
}

// clientWantsDO 报告客户端请求是否携带 DO 位（请求 DNSSEC 记录）。
func clientWantsDO(m *dns.Msg) bool {
	o := m.IsEdns0()
	return o != nil && o.Do()
}

// setDO 在 msg 上设置 EDNS0 的 DO 位（表示请求 DNSSEC 记录）；若无 OPT 则新建。
// 仅透传 DO 位，不做签名验证。
func setDO(m *dns.Msg) {
	if o := m.IsEdns0(); o != nil {
		if !o.Do() {
			o.SetDo()
		}
		return
	}
	// 1232 为 RFC 8466 推荐的 UDP 载荷，避免 IPv6 路径上的分片。
	m.SetEdns0(1232, true)
}

var _ proxy.Handler = (*Handler)(nil)
