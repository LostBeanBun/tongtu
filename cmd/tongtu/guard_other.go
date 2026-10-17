//go:build !windows

package main

import "syscall"

// guardSysProcAttr 非 Windows 平台不需要额外的创建标志。
func guardSysProcAttr(bool) *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// waitProcessExit 非 Windows 平台无法用一个句柄等进程退出，直接返回 false
// （调用方会立即执行还原）。
func waitProcessExit(int) bool { return false }

// processAlive 非 Windows 平台没有「别的实例仍持有系统状态」这回事。
func processAlive(int) bool { return false }
