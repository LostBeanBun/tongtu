//go:build windows

package main

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestWaitProcessExitAccessDeniedKeepsWaiting 覆盖实测缺陷12 的触发点：
// OpenProcess(SYNCHRONIZE) 因权限失败时，进程其实还活着——必须按 processAlive 的口径
// 轮询等它退出，绝不能当成「已退出」立即跑去还原（那会清掉活着实例的兜底）。
func TestWaitProcessExitAccessDeniedKeepsWaiting(t *testing.T) {
	oldOpen, oldAlive, oldInterval := openForWait, aliveForWait, waitPollInterval
	t.Cleanup(func() {
		openForWait, aliveForWait, waitPollInterval = oldOpen, oldAlive, oldInterval
	})

	openForWait = func(int) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	waitPollInterval = time.Millisecond
	checks := 0
	aliveForWait = func(int) bool { checks++; return checks <= 3 }

	if !waitProcessExit(4242) {
		t.Fatal("打不开句柄但进程活着时应继续等待，不能当成已退出")
	}
	if checks <= 3 {
		t.Errorf("应持续轮询到进程退出，实际只检查了 %d 次", checks)
	}
}

// TestWaitProcessExitGoneImmediately 覆盖「真的没了」：句柄打不开且进程确认不存在时
// 返回 false，调用方立即还原。
func TestWaitProcessExitGoneImmediately(t *testing.T) {
	oldOpen, oldAlive := openForWait, aliveForWait
	t.Cleanup(func() { openForWait, aliveForWait = oldOpen, oldAlive })

	openForWait = func(int) (windows.Handle, error) { return 0, windows.ERROR_INVALID_PARAMETER }
	aliveForWait = func(int) bool { return false }

	if waitProcessExit(4242) {
		t.Error("确认进程不在时应返回 false（调用方立即还原）")
	}
	if waitProcessExit(0) {
		t.Error("pid<=0 时应返回 false")
	}
}
