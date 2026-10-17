//go:build windows

package main

import "golang.org/x/sys/windows"

// 通途界面用到的 Win32 共享句柄与模块。
// swHide / procShowWindow 用于「点 × 只隐藏窗口」（见 window_windows.go 的 WM_CLOSE）。
// 判断「有没有控制台可依附」不用 GetConsoleWindow：无头/非交互式窗口站下它一律返回
// NULL，而进程其实已经连上控制台——改用 GetConsoleProcessList（见 console_windows.go）。
const swHide = 0

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procShowWindow = user32.NewProc("ShowWindow")
)
