//go:build windows

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// CreateProcess 标志（见 MSDN）。
const (
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

// stillActive 是 GetExitCodeProcess 表示「进程还在跑」的返回值（STILL_ACTIVE）。
const stillActive = 259

// guardSysProcAttr 返回监护进程的启动属性。
//
//   - CREATE_NO_WINDOW：不给监护进程创建控制台（通途本身是 GUI 子系统，也没有）；
//   - CREATE_BREAKAWAY_FROM_JOB：万一主进程运行在一个「关闭即杀掉全部子进程」的作业
//     对象里（例如某些终端/IDE 的进程管理），监护进程也能脱离出来，继续完成还原；
//     作业对象不允许脱离时 CreateProcess 会失败，调用方会退回不带该标志的重试。
func guardSysProcAttr(breakaway bool) *syscall.SysProcAttr {
	flags := uint32(createNoWindow)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	return &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}

// waitProcessExit 等待 pid 对应的进程退出。
//
// 返回 true 表示「成功等到它退出」（或者它在我们等待期间退出）；
// 返回 false 表示拿不到进程句柄——最常见的原因是进程已经不在了，
// 此时调用方应当立即执行还原。
//
// 之所以等「进程句柄」而不是轮询进程是否存在：句柄在进程结束的瞬间就进入已通知状态，
// 不受 PID 复用影响，也不会有轮询间隔带来的延迟。
func waitProcessExit(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return false
	}
	return true
}

// processAlive 报告 pid 是否还有一个活着的进程。
//
// 用在「遗留清理」之前的安全检查上（见 foreignLiveSession）：上一轮实例的主进程还活着，
// 系统代理 / hosts 就归它管，我们不能去清。
//
// 权限注意：对方可能是提权运行的实例，此时 OpenProcess 会以 ACCESS_DENIED 失败。
// 这种「打不开」必须按「还活着」处理——否则非提权启动的第二个实例会把提权实例的
// 系统代理清空，而提权实例自己还以为接管着。只有 ERROR_INVALID_PARAMETER
// （pid 根本不存在）才算真的退出了。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
