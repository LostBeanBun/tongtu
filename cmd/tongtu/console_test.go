//go:build windows

package main

import "testing"

// TestConsoleCtrlIsClose 关闭/注销/关机事件需要优雅还原；Ctrl+C / Ctrl+Break 交给 os/signal。
func TestConsoleCtrlIsClose(t *testing.T) {
	const (
		ctrlCEvent     = 0
		ctrlBreakEvent = 1
	)
	for _, c := range []uint32{ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent} {
		if !consoleCtrlIsClose(c) {
			t.Fatalf("控制台事件 %d 应触发关闭清理", c)
		}
	}
	for _, c := range []uint32{ctrlCEvent, ctrlBreakEvent, 99} {
		if consoleCtrlIsClose(c) {
			t.Fatalf("控制台事件 %d 不应触发关闭清理", c)
		}
	}
}
