package app

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"tongtu/internal/config"
	"tongtu/internal/rules"
)

// newPauseTestApp 起一个不接管系统代理 / hosts / 反代的实例（只监听回环随机端口）。
//
// 定时暂停的测试只关心「计时与代号」这一层，因此不真的去改系统设置：
// 到期后的恢复动作由 pauseExpireHook 替换成记录调用（见各用例）。
func newPauseTestApp(t *testing.T) *App {
	t.Helper()
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.Hosts, cfg.MITM.Enabled = false, false, false
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a
}

// stubPauseExpire 把「到期恢复」替换成计数，返回计数函数。
func stubPauseExpire(t *testing.T) func() int32 {
	t.Helper()
	var n int32
	old := pauseExpireHook
	pauseExpireHook = func(*App) error {
		atomic.AddInt32(&n, 1)
		return nil
	}
	t.Cleanup(func() { pauseExpireHook = old })
	return func() int32 { return atomic.LoadInt32(&n) }
}

// markPaused 把实例置为「已暂停」，模拟 StopAll 之后的状态。
//
// 直接改内部状态而不是调用 StopAll：后者在没有任何接管项时不会进入暂停态，
// 而定时暂停要测的正是「已经暂停了，再设定时」这一条路径。
func markPaused(a *App) {
	a.mu.Lock()
	a.paused = &pauseState{mitm: true}
	a.mu.Unlock()
}

// TestPauseForAutoResumes 定时暂停到点会触发恢复，并在状态里报告到期时间。
func TestPauseForAutoResumes(t *testing.T) {
	a := newPauseTestApp(t)
	fired := stubPauseExpire(t)
	markPaused(a)

	if err := a.PauseFor(40 * time.Millisecond); err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	st := a.Status()
	if !st.Paused {
		t.Fatal("设定定时暂停后应处于暂停态")
	}
	if st.PauseUntil == 0 {
		t.Fatal("状态里应有到期时间")
	}
	if _, ok := a.PauseDeadline(); !ok {
		t.Fatal("PauseDeadline 应报告定时暂停的到期时间")
	}

	waitFor(t, 3*time.Second, func() bool { return fired() == 1 })
}

// TestManualResumeCancelsPauseTimer 手动恢复之后，旧定时器不能再触发一次。
func TestManualResumeCancelsPauseTimer(t *testing.T) {
	a := newPauseTestApp(t)
	fired := stubPauseExpire(t)
	markPaused(a)

	if err := a.PauseFor(60 * time.Millisecond); err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	// 用户点了「恢复」：暂停项已经装回去，这里只需要清掉暂停记录，
	// 与 Resume 的记账一致（不碰系统设置，避免测试去改真实代理 / hosts）。
	a.mu.Lock()
	a.paused = nil
	a.mu.Unlock()
	if err := a.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, ok := a.PauseDeadline(); ok {
		t.Fatal("手动恢复后不该再有到期时间")
	}

	time.Sleep(200 * time.Millisecond)
	if n := fired(); n != 0 {
		t.Fatalf("手动恢复后旧定时器仍触发了 %d 次", n)
	}
}

// TestPauseForRepanArmsNewTimer 暂停中途改时长：只有最后一次生效，且只恢复一次。
func TestPauseForRepanArmsNewTimer(t *testing.T) {
	a := newPauseTestApp(t)
	fired := stubPauseExpire(t)
	markPaused(a)

	if err := a.PauseFor(40 * time.Millisecond); err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	if err := a.PauseFor(250 * time.Millisecond); err != nil {
		t.Fatalf("再次 PauseFor: %v", err)
	}

	time.Sleep(120 * time.Millisecond)
	if n := fired(); n != 0 {
		t.Fatalf("第一次的短定时器不该再触发（触发 %d 次）", n)
	}
	waitFor(t, 3*time.Second, func() bool { return fired() == 1 })
	// 再等一会儿，确认没有第二个定时器补上一枪
	time.Sleep(150 * time.Millisecond)
	if n := fired(); n != 1 {
		t.Fatalf("定时恢复应只发生一次，实际 %d 次", n)
	}
}

// TestPauseForZeroKeepsManualPause 不指定时长（d<=0）时不设定时器。
func TestPauseForZeroKeepsManualPause(t *testing.T) {
	a := newPauseTestApp(t)
	fired := stubPauseExpire(t)

	if err := a.PauseFor(0); err != nil {
		t.Fatalf("PauseFor(0): %v", err)
	}
	if _, ok := a.PauseDeadline(); ok {
		t.Fatal("没有指定时长时不该有到期时间")
	}
	if a.Status().Paused {
		t.Fatal("没有任何接管项时暂停是空操作，不该进入暂停态")
	}
	time.Sleep(80 * time.Millisecond)
	if n := fired(); n != 0 {
		t.Fatalf("不该有定时器触发，实际 %d 次", n)
	}
}

// waitFor 轮询等待条件成立（定时器测试用；失败时给出明确信息）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
