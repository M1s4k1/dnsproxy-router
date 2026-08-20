// Package handler 实现 proxy.Handler，把调度器的选路注入每个请求。
package handler

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/AdguardTeam/dnsproxy/proxy"

	"dnsproxy-scheduler/internal/ecs"
	"dnsproxy-scheduler/internal/scheduler"
)

// Handler 把 scheduler 当前周期的选路（各家最优线路 + 缓存）注入每个请求，
// 再交还 p.Resolve：既保留库的缓存、去重、SERVFAIL 兜底，
// 又实现「多家并发赛马」（依赖 proxy.UpstreamModeParallel）。
type Handler struct {
	sched          *scheduler.Scheduler
	logger         *slog.Logger
	ecs            *ecs.Policy
	fallbackLogged atomic.Bool
}

func New(sched *scheduler.Scheduler, logger *slog.Logger, ecs *ecs.Policy) *Handler {
	return &Handler{sched: sched, logger: logger, ecs: ecs}
}

func (h *Handler) logOnce(msg string) {
	if h.fallbackLogged.CompareAndSwap(false, true) {
		h.logger.Warn(msg)
	}
}

func (h *Handler) ServeDNS(ctx context.Context, p *proxy.Proxy, dctx *proxy.DNSContext) error {
	cfg := h.sched.CurrentConfig()
	if cfg == nil {
		// 选路尚未就绪（首轮探测未完成，或所有上游均不可用）。
		// 仅在选路为 nil 的整段期间提示一次，避免每个请求刷屏。
		h.logOnce("选路尚未就绪，回退默认上游")
		return proxy.DefaultHandler{}.ServeDNS(ctx, p, dctx)
	}

	h.fallbackLogged.Store(false)

	// 按 ECS 策略修改发往上游的请求（pass 模式由库透传，无需在此处理）。
	h.ecs.Apply(dctx.Req)

	dctx.CustomUpstreamConfig = cfg
	return p.Resolve(ctx, dctx)
}

var _ proxy.Handler = (*Handler)(nil)
