//go:build !windows

package main

import (
	"errors"
	"io"
)

// hasLogWindow 表示当前平台是否提供独立的日志窗口（本实现仅 Windows 有 UI）。
const hasLogWindow = false

// lw 在非 Windows 平台上丢弃日志：没有独立窗口，日志只走控制台与日志文件。
var lw io.Writer = io.Discard

// logDisplay 非 Windows 平台没有日志窗口，返回 nil（日志处理器会跳过窗口这一路）。
func logDisplay() logSink { return nil }

var errNoLogWindow = errors.New("当前平台没有独立的日志窗口")

func showLogWindow() error { return errNoLogWindow }

// prepareLogWindow 非 Windows 平台没有窗口需要预先创建。
func prepareLogWindow() error { return errNoLogWindow }

func toggleLogWindow() error { return errNoLogWindow }

// setWindowStatus 非 Windows 平台没有窗口，空实现。
func setWindowStatus(windowStatus) {}

// setSessionEndHandler 非 Windows 平台没有窗口消息，注册后不会被调用。
func setSessionEndHandler(func()) {}

// setPowerResumeHandler 非 Windows 平台没有窗口消息，注册后不会被调用。
func setPowerResumeHandler(func(string)) {}

// setDPIAware 非 Windows 平台不需要声明 DPI 感知，空实现。
func setDPIAware() {}
