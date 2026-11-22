package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ensureGuardDir 保证数据目录存在：Linux CI 里 APPDATA 为空，
// config.Dir() 会落到相对路径 ./tongtu，直接写 guard.json 会报 no such file。
func ensureGuardDir(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(guardStatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestFinishGuardCleanupKeepsRunOnceWhileStateHeld 覆盖实测缺陷12：
// RecoverStale 因为「状态仍由活着的持有者掌管」而收手时，监护进程绝不能撤 RunOnce、
// 也不能删 guard.json——否则持有者的掉电兜底与崩溃防护会同时失效。
func TestFinishGuardCleanupKeepsRunOnceWhileStateHeld(t *testing.T) {
	ensureGuardDir(t)
	restore := stashGuardState(t)
	defer restore()
	writeGuardState(t, guardInfo{PID: 1, Token: "t", At: time.Now()})

	oldActive := stateActive
	stateActive = func() bool { return true }
	t.Cleanup(func() { stateActive = oldActive })

	f := &fakeRunOnce{cur: "recover-cmd"}
	f.install(t)

	finishGuardCleanup()

	if f.clears != 0 || f.cur == "" {
		t.Errorf("系统状态仍被持有，不应撤 RunOnce（cur=%q clears=%d）", f.cur, f.clears)
	}
	if _, err := os.Stat(guardStatePath()); err != nil {
		t.Error("系统状态仍被持有，不应删除 guard.json")
	}
}

// TestFinishGuardCleanupClearsWhenStateGone 覆盖正常收尾：状态文件确实没了，
// 撤 RunOnce、回收令牌。
func TestFinishGuardCleanupClearsWhenStateGone(t *testing.T) {
	ensureGuardDir(t)
	restore := stashGuardState(t)
	defer restore()
	writeGuardState(t, guardInfo{PID: 1, Token: "t", At: time.Now()})

	oldActive := stateActive
	stateActive = func() bool { return false }
	t.Cleanup(func() { stateActive = oldActive })

	f := &fakeRunOnce{cur: "recover-cmd"}
	f.install(t)

	finishGuardCleanup()

	if f.clears != 1 || f.cur != "" {
		t.Errorf("状态已还原应撤掉 RunOnce（cur=%q clears=%d）", f.cur, f.clears)
	}
	if _, err := os.Stat(guardStatePath()); !os.IsNotExist(err) {
		t.Errorf("状态已还原应回收 guard.json（stat err=%v）", err)
	}
}

// TestShouldDisarmRunOnce 覆盖实测缺陷13① 的门控：只有系统里确实没有待还原状态时
// 才允许撤 RunOnce；另一个活着的实例持有时必须收手。
func TestShouldDisarmRunOnce(t *testing.T) {
	oldActive := stateActive
	t.Cleanup(func() { stateActive = oldActive })

	stateActive = func() bool { return true }
	if shouldDisarmRunOnce() {
		t.Error("系统里还有待还原状态（另一个实例持有）时不应撤 RunOnce")
	}
	stateActive = func() bool { return false }
	if !shouldDisarmRunOnce() {
		t.Error("没有待还原状态时应允许撤 RunOnce")
	}
}

// TestGuardStartupDecisions 覆盖实测缺陷13② 的启动决策：
// 另一个实例活着时不能占令牌（不能覆盖它的 guard.json），
// 但若它随后退出、本轮接管成功，必须补占令牌，保证「持有状态 ⇒ 有监护」。
func TestGuardStartupDecisions(t *testing.T) {
	if shouldClaimGuard(true) {
		t.Error("另一个实例活着时不应占用本轮令牌（会覆盖它的 guard.json）")
	}
	if !shouldClaimGuard(false) {
		t.Error("没有别的实例时应占用本轮令牌")
	}

	restoreToken := setGuardToken(t, "")
	defer restoreToken()

	if !needLateClaim(true, true) {
		t.Error("启动时没占令牌、本轮又接管成功，应补占令牌")
	}
	if needLateClaim(false, true) {
		t.Error("本轮不接管系统代理时不该补占令牌")
	}
	if needLateClaim(true, false) {
		t.Error("本轮没接管成功时不该补占令牌")
	}

	setGuardToken(t, "tok")
	if !guardClaimed() {
		t.Error("已经占过令牌时 guardClaimed 应为真")
	}
	if needLateClaim(true, true) {
		t.Error("已经占过令牌时不应重复占用")
	}
}
