//go:build !windows

package main

// 非 Windows 平台没有自绘窗口，控制端口的 /ui/layout 因此返回 404。
// （真实实现见 ui_snapshot_windows.go。）
func uiLayoutSnapshot() any { return nil }
