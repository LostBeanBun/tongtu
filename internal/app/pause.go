package app

// pause.go 实现「定时暂停」：暂停加速 N 分钟后自动恢复。
//
// 为什么放在 app 层而不是界面里：托盘、窗口、控制 API 都会用到同一个暂停状态，
// 定时器如果挂在界面线程上，窗口隐藏时（只剩托盘）就不会到期；而暂停状态本来
// 就由 App 持有（见 app.go 的 pauseState），计时也跟着它走才不会有第二份真相。
//
// 语义上只有两种暂停：
//   - 手动暂停（StopAll）：一直停到用户点「恢复」；
//   - 定时暂停（PauseFor）：到点自动恢复，用户也可以提前手动恢复。
//
// 两者共用同一套「撤掉 / 装回」逻辑（stopAllLocked / resumeLocked），定时只是
// 在暂停之后额外挂一个定时器。

import (
	"sync"
	"time"
)

// 可被单测替换的两个钩子集中放在 hookMu 下：定时器协程到期时读它们，
// 测试在结束时写回（还原默认值）。读与写必须在同一把锁下，否则两者之间
// 没有 happens-before 边——CI 的 Linux -race 抓到过一次
// （pause_test.go 的写回 vs autoResume 里日志读 autoResumeRetry）。
var (
	hookMu sync.Mutex
	// pauseExpireHook 是定时暂停到期时执行的恢复动作。
	//
	// 单独抽出来是为了单测：真机上「恢复」会去改系统代理，测试里不能那么做。
	// 生产路径是 resumeLocked（与手动恢复同一套实现，只是日志里写明触发原因）。
	pauseExpireHook = func(a *App) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.resumeLocked("定时暂停到期")
	}
	// autoResumeRetry 是「到期自动恢复失败」之后的重试间隔。
	//
	// 恢复可能失败（本地 CA 读不出、根证书被移除、注册表写失败）：失败后暂停记录还在，
	// 不重挂定时器就成了死暂停——「暂停 30 分钟」永远等不到自动恢复。做成变量是
	// 给单测留的（真机上不能真等一分钟）。
	autoResumeRetry = time.Minute
)

// currentHooks 返回当前的到期恢复动作与失败重试间隔（并发安全）。
func currentHooks() (func(*App) error, time.Duration) {
	hookMu.Lock()
	defer hookMu.Unlock()
	return pauseExpireHook, autoResumeRetry
}

// setPauseHooks 替换两个钩子并返回还原函数（并发安全，测试用）。
func setPauseHooks(h func(*App) error, retry time.Duration) func() {
	hookMu.Lock()
	oldH, oldR := pauseExpireHook, autoResumeRetry
	pauseExpireHook, autoResumeRetry = h, retry
	hookMu.Unlock()
	return func() {
		hookMu.Lock()
		pauseExpireHook, autoResumeRetry = oldH, oldR
		hookMu.Unlock()
	}
}

// PauseFor 暂停加速，并在 d 之后自动恢复；d <= 0 表示一直暂停到手动恢复（等同 StopAll）。
//
// 已经处于暂停状态时调用它只改「到期时间」，不会重新撤一遍（避免重复还原系统设置）；
// 反过来，任何一次新的暂停都会作废旧定时器（见 pauseGen）。
func (a *App) PauseFor(d time.Duration) error {
	if d <= 0 {
		return a.StopAll()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	var err error
	if !a.paused.active() {
		err = a.stopAllLocked()
	}
	if !a.paused.active() {
		// 没有任何接管项（例如配置里三个开关全关）：不设定时器。
		// 否则到点会「自动恢复」一次什么都没撤的暂停，日志里凭空多一条。
		return err
	}

	a.pauseGen++
	gen := a.pauseGen
	a.pauseUntil = time.Now().Add(d)
	time.AfterFunc(d, func() { a.autoResume(gen) })
	a.log.Info("已暂停加速，将在到期后自动恢复", "after", d.String())
	return err
}

// autoResume 是定时器到期的入口：只有「代号仍然有效」时才真的恢复。
//
// 代号在每次暂停 / 恢复 / 退出时都会加一，因此被手动恢复或重新计时作废的旧定时器
// 醒来后什么都不做——否则会出现「暂停 30 分钟，中途手动恢复，30 分钟后又被恢复一次」
// 这种在日志里很难解释的行为。
func (a *App) autoResume(gen int64) {
	a.mu.Lock()
	if gen != a.pauseGen || !a.paused.active() {
		a.mu.Unlock()
		return
	}
	a.pauseGen++
	a.pauseUntil = time.Time{}
	a.mu.Unlock()

	hook, retry := currentHooks()
	if err := hook(a); err != nil {
		// 恢复失败（CA 读不出 / 根证书被移除 / 注册表写失败……）：暂停记录仍有效，
		// 必须重挂定时器，否则倒计时消失、又没有任何定时器在跑——死暂停，
		// 用户只能干等。1 分钟后重试；期间用户手动恢复或重新计时都会让代号作废，
		// 迟到的旧定时器醒来即退（见 autoResume 的代号校验）。
		a.rearmAutoResume(retry)
		a.log.Warn("定时暂停到期，自动恢复未完全成功，稍后自动重试",
			"err", err, "retry", retry.String())
		return
	}
	a.CheckHealth()
}

// rearmAutoResume 恢复失败后重新挂上自动恢复定时器（需在 a.mu 外调用）。
//
// 只在暂停记录仍有效时重挂：用户若已手动恢复（记录已清），这次重试就是多余的，
// 直接收手——代号校验兜底，迟到的定时器绝不会把已恢复的暂停再拉起来。
func (a *App) rearmAutoResume(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.paused.active() {
		return
	}
	a.pauseGen++
	gen := a.pauseGen
	a.pauseUntil = time.Now().Add(d)
	time.AfterFunc(d, func() { a.autoResume(gen) })
}

// PauseDeadline 返回定时暂停的到期时间；未定时（手动暂停或没暂停）时 ok=false。
func (a *App) PauseDeadline() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.paused.active() || a.pauseUntil.IsZero() {
		return time.Time{}, false
	}
	return a.pauseUntil, true
}
