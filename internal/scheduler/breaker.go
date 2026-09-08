package scheduler

import (
	"log/slog"
	"sync"
	"time"
)

// breaker 是挂在聚合上游上的轻量熔断器：按成员下标记录连续失败次数，连续失败
// 达到阈值即熔断（open），冷却期内不再调度该成员，实现「快速失败」；冷却到点
// 自动放行一次半开探测，成功即关闭熔断。
type breaker struct {
	failThreshold int
	cooldown      time.Duration
	now           func() time.Time
	logger        *slog.Logger

	mu     sync.Mutex
	states []breakerState // 与成员下标对齐
}

// breakerState 是单个成员的熔断状态。
type breakerState struct {
	consecutive int
	openedAt    time.Time // zero = 未熔断
}

// newBreaker 构造熔断器。n 为成员数；failThreshold 为熔断阈值；cooldown 为冷却时长。
func newBreaker(n, failThreshold int, cooldown time.Duration, logger *slog.Logger) *breaker {
	return &breaker{
		failThreshold: failThreshold,
		cooldown:      cooldown,
		now:           time.Now,
		logger:        logger,
		states:        make([]breakerState, n),
	}
}

// pick 返回本轮应参与查询的成员下标：剔除仍处于冷却期的熔断成员。冷却到点的
// 成员自然重新进入候选（半开探测），成功即由 report 关闭熔断。全部冷却中时
// 返回空，调用方快速失败（ErrNoReply），避免打故障上游。
func (b *breaker) pick(n int) []int {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	idx := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if b.openLocked(i, now) {
			continue
		}
		idx = append(idx, i)
	}
	return idx
}

// report 记录成员 i 的一次结果：成功清零；失败累加，达到阈值则置 openedAt 熔断。
func (b *breaker) report(i int, err error) {
	if i < 0 || i >= len(b.states) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := &b.states[i]
	if err == nil {
		if st.openedAt.IsZero() && st.consecutive == 0 {
			return
		}
		if !st.openedAt.IsZero() && b.logger != nil {
			b.logger.Warn("上游熔断恢复", "member", i)
		}
		st.consecutive = 0
		st.openedAt = time.Time{}
		return
	}

	st.consecutive++
	if st.consecutive >= b.failThreshold {
		st.openedAt = b.now()
		if b.logger != nil {
			b.logger.Warn("上游连续失败，触发熔断", "member", i, "consecutive", st.consecutive)
		}
	}
}

// openLocked 报告成员 i 当前是否处于熔断中。已持有锁。
func (b *breaker) openLocked(i int, now time.Time) bool {
	st := b.states[i]
	if st.openedAt.IsZero() {
		return false
	}
	return now.Sub(st.openedAt) < b.cooldown
}
