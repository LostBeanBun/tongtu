//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// guardSysProcAttr 非 Windows 平台不需要额外的创建标志。
func guardSysProcAttr(bool) *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// waitProcessExit 非 Windows 平台无法用一个句柄等进程退出，直接返回 false
// （调用方会立即执行还原）。
func waitProcessExit(int) bool { return false }

// processAlive 判断进程是否还活着。监护进程本身只在 Windows 上启动，
// 但 recycleStaleGuard / foreignGuardOwner 是跨平台可测的纯逻辑，
// 这里按 Unix 语义实现，测试才不会因为「非 Windows 一律当死进程」而误报。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0 不发送信号，只做存在性检查；EPERM 说明进程存在但属于别的用户。
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// inJobObject 非 Windows 平台没有作业对象。
func inJobObject() bool { return false }
