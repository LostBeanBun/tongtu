//go:build windows

package main

import (
	"errors"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreateProcess 标志（见 MSDN）。
const (
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

// inJobObject 报告当前进程是否运行在作业对象里。
//
// 用途：监护进程「无法脱离作业对象」时（CreateProcess 带 CREATE_BREAKAWAY_FROM_JOB 失败），
// 说明所处的作业对象不允许脱离——崩溃/强杀时监护进程会跟着主进程一起结束，
// 日志里带上它便于判断到底是「作业对象限制」还是别的原因。
func inJobObject() bool {
	var in int32
	r, _, _ := kernel32.NewProc("IsProcessInJob").Call(
		uintptr(windows.CurrentProcess()),
		0,
		uintptr(unsafe.Pointer(&in)))
	return r != 0 && in != 0
}

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

// waitPollInterval 是「拿不到句柄」时轮询进程存活状态的间隔（测试可调小）。
var waitPollInterval = 500 * time.Millisecond

// openForWait 是 OpenProcess(SYNCHRONIZE) 的注入点（测试模拟「活着但打不开」）。
var openForWait = func(pid int) (windows.Handle, error) {
	return windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
}

// aliveForWait 是轮询存活检查的注入点（测试用）。
var aliveForWait = processAlive

// waitProcessExit 等待 pid 对应的进程退出。
//
// 返回 true 表示「一直等到它退出」；返回 false 表示拿不到句柄、且已确认进程不在
// （调用方应当立即执行还原）。
//
// 拿不到句柄时必须区分两种情况：进程真的没了（ERROR_INVALID_PARAMETER），还是
// 活着但权限打不开（ACCESS_DENIED）。后者绝不能当成「已退出」——否则监护进程会在
// 主进程还活着的时候就跑去「还原」，把正在生效的断电自愈项与监护令牌一起清掉，
// 等主进程真的崩溃时已经没人能还原（实测缺陷12）。口径与 processAlive 保持一致：
// 打不开就按活着处理，轮询等它退出。
//
// 之所以优先等「进程句柄」而不是轮询进程是否存在：句柄在进程结束的瞬间就进入已通知状态，
// 不受 PID 复用影响，也不会有轮询间隔带来的延迟。
func waitProcessExit(pid int) bool {
	if pid <= 0 {
		return false
	}
	if h, err := openForWait(pid); err == nil {
		defer windows.CloseHandle(h)
		if _, werr := windows.WaitForSingleObject(h, windows.INFINITE); werr == nil {
			return true
		}
		// 等待失败（极少见）：下面退化为轮询，别把「活着」误判成「已退出」。
	}
	if !aliveForWait(pid) {
		return false
	}
	for aliveForWait(pid) {
		time.Sleep(waitPollInterval)
	}
	return true
}

// processAlive 报告 pid 是否还有一个活着的进程。
//
// 用在「遗留清理」之前的安全检查上（见 foreignLiveSession）：上一轮实例的主进程还活着，
// 系统代理就归它管，我们不能去清。
//
// 权限注意：对方可能运行在更高权限的会话里，此时 OpenProcess 会以 ACCESS_DENIED 失败。
// 这种「打不开」必须按「还活着」处理——否则第二个实例会把还在接管中的
// 系统代理清空，而对方自己还以为接管着。只有 ERROR_INVALID_PARAMETER
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
