//go:build !windows

package main

import "os"

// 非 Windows 平台沿用标准控制台行为（本实现主要面向 Windows）。

// ensureConsole 非 Windows 平台始终可用（进程本身就有标准输出）。
func ensureConsole() bool {
	_, err := os.Stdout.Stat()
	return err == nil
}

// consoleAvailable 报告标准输出是否可用。
func consoleAvailable() bool {
	_, err := os.Stdout.Stat()
	return err == nil
}

// detachConsole 非 Windows 平台没有「控制台关闭会强杀进程」的问题，空实现。
func detachConsole() bool { return false }

// consoleAttached 非 Windows 平台上进程本身就带着标准终端。
func consoleAttached() bool { return true }

// messageBox 非 Windows 平台没有系统对话框，返回「确定」。
func messageBox(string, string, uint32) int32 { return 0 }

// installConsoleCloseHandler 非 Windows 平台不注册控制台事件。
func installConsoleCloseHandler(func()) {}

const (
	mbOK              = 0
	mbIconError       = 0
	mbIconInformation = 0
	mbYesNo           = 0
	mbIconQuestion    = 0
	mbSetForeground   = 0
	mbTopMost         = 0
	messageBoxYes     = 6
)
