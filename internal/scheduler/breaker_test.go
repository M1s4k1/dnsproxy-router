package scheduler

import (
	"errors"
	"testing"
	"time"
)

// newTestBreaker 构造阈值 2、冷却 10s、时钟可控的熔断器。
func newTestBreaker(t *testing.T) (*breaker, *time.Time) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	b := newBreaker(3, 2, 10*time.Second, nil)
	b.now = func() time.Time { return base }
	return b, &base
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	b, _ := newTestBreaker(t)

	// 首次失败未达阈值，仍参与。
	b.report(0, errors.New("x"))
	if got := b.pick(1); len(got) != 1 || got[0] != 0 {
		t.Fatalf("阈值前应仍参与，得到 %v", got)
	}

	// 第二次失败触发熔断。
	b.report(0, errors.New("x"))
	if got := b.pick(1); len(got) != 0 {
		t.Fatalf("熔断后应被剔除，得到 %v", got)
	}
}

func TestBreakerRecoversAfterCooldown(t *testing.T) {
	b, base := newTestBreaker(t)

	b.report(0, errors.New("x"))
	b.report(0, errors.New("x"))
	if got := b.pick(1); len(got) != 0 {
		t.Fatalf("熔断后应被剔除，得到 %v", got)
	}

	// 推进到冷却结束：重新放行，成功一次即关闭。
	*base = base.Add(11 * time.Second)
	if got := b.pick(1); len(got) != 1 {
		t.Fatalf("冷却后应放行，得到 %v", got)
	}
	b.report(0, nil)

	// 关闭后再次失败仅计 1 次，未达阈值，仍参与。
	b.report(0, errors.New("x"))
	if got := b.pick(1); len(got) != 1 {
		t.Fatalf("关闭后单次失败不应熔断，得到 %v", got)
	}
}

func TestBreakerAllOpenReturnsEmpty(t *testing.T) {
	b, _ := newTestBreaker(t)

	// 3 个成员全部熔断，仍在冷却期内：pick 应返回空，触发快速失败。
	for i := 0; i < 3; i++ {
		b.report(i, errors.New("x"))
		b.report(i, errors.New("x"))
	}
	if got := b.pick(3); len(got) != 0 {
		t.Fatalf("全部冷却中应返回空（快速失败），得到 %v", got)
	}
}

func TestBreakerSuccessResets(t *testing.T) {
	b, _ := newTestBreaker(t)

	b.report(0, errors.New("x"))
	b.report(0, nil)
	// 成功清零：再失败一次不应触发（阈值 2）。
	b.report(0, errors.New("x"))
	if got := b.pick(1); len(got) != 1 {
		t.Fatalf("成功清零后单次失败不应熔断，得到 %v", got)
	}
}
