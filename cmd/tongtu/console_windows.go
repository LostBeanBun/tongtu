//go:build windows

package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 控制台控制事件（wincon.h）。
const (
	ctrlCloseEvent    = 2
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

// ATTACH_PARENT_PROCESS：把本进程接到父进程的控制台上。
const attachParentProcess = ^uintptr(0)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	consoleClose              func()

	// consoleGone 在主动脱离控制台后置位：此后不应再往（可能已经消失的）控制台
	// 写日志或等用户输入，错误提示改走系统对话框。
	consoleGone atomic.Bool
)

// ensureConsole 尽力让标准输出/输入可用，返回是否「有可用的控制台」。
//
// 通途以 GUI 子系统编译（见 build.ps1 的 -H windowsgui）：双击时 Windows 不会
// 创建 cmd 窗口，也就没有「关掉 cmd 窗口 → 进程被结束」的问题。代价是标准句柄
// 默认无效，因此从终端运行时需要显式接到父进程的控制台上：
//
//   - 从 cmd / PowerShell / Windows Terminal 运行：附加父进程控制台，日志照常打在终端里；
//   - 双击 / 开机自启：父进程（资源管理器、计划任务）没有控制台，附加失败，
//     此时界面完全由通途自己的窗口与托盘承担；
//   - 标准句柄已被父进程重定向（管道 / 文件）：保持不动，直接用重定向的输出。
func ensureConsole() bool {
	if consoleAvailable() {
		return true
	}
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return false
	}
	rebindStdHandles()
	return consoleAvailable()
}

// consoleAvailable 报告当前是否已有可用的控制台或已被重定向的标准输出。
// 用于决定「错误提示打在终端里」还是「弹系统对话框」。
func consoleAvailable() bool {
	if consoleGone.Load() {
		return false
	}
	if consoleAttached() {
		return true
	}
	_, err := os.Stdout.Stat() // 被父进程重定向到管道 / 文件时也算可用
	return err == nil
}

// consoleAttached 报告本进程是否真的连着一个控制台。
//
// 不能用 GetConsoleWindow 判断：在没有控制台窗口的环境里（无头会话、某些
// 服务/窗口站），即使进程确实挂在控制台上，它也只会返回 NULL。GetConsoleProcessList
// 是判断「是否已连接控制台」的官方办法：未连接时返回 0。
func consoleAttached() bool {
	var pids [2]uint32
	r, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), 2)
	return r > 0
}

// rebindStdHandles 在附加控制台之后重新打开 CONOUT$/CONIN$。
// Go 启动时拿到的句柄是无效的，必须重新打开；已被重定向的句柄保持原样。
func rebindStdHandles() {
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		if _, err := os.Stdout.Stat(); err != nil {
			os.Stdout = f
		}
		if _, err := os.Stderr.Stat(); err != nil {
			os.Stderr = f
		}
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		if _, err := os.Stdin.Stat(); err != nil {
			os.Stdin = f
		}
	}
}

// detachConsole 解除与控制台的关联，返回是否真的分离了。
//
// 这一步是「关闭终端不再影响通途」的关键：只要进程还挂在控制台上，用户关掉那个
// 控制台窗口（Windows Terminal / conhost）时，系统会发送 CTRL_CLOSE_EVENT，
// 并在处理函数返回后**强制结束进程**（返回「已处理」也拦不住）。分离之后，
// 控制台关闭与本进程再无关系，通途照常在托盘里运行。
//
// 由于通途是 GUI 子系统程序，双击启动时本来就没有控制台；这里主要处理
// 「从终端启动」与「终端里 Ctrl+C 之后」的场景。附着的控制台若还有别的进程
// （比如用户的 shell），分离不会关闭那个窗口。
func detachConsole() bool {
	if consoleGone.Load() {
		return false
	}
	if !consoleAttached() {
		consoleGone.Store(true)
		return false
	}
	r, _, _ := procFreeConsole.Call()
	consoleGone.Store(true)
	return r != 0
}

// messageBox 弹出系统对话框（GUI 模式下没有终端，错误与询问都靠它）。
const (
	mbOK           = 0x00000000
	mbIconError    = 0x00000010
	mbYesNo        = 0x00000004
	mbIconQuestion = 0x00000020
	messageBoxYes  = 6
)

func messageBox(title, text string, flags uint32) int32 {
	procMessageBoxW := user32.NewProc("MessageBoxW")
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), uintptr(flags))
	return int32(r)
}

// installConsoleCloseHandler 注册控制台事件处理：用户关闭控制台窗口 / 注销 / 关机时，
// 触发一次优雅停止（还原系统代理与 hosts），避免留下指向已停止代理的失效设置。
// Ctrl+C / Ctrl+Break 不在本处理范围内，仍由 os/signal 处理。
//
// 正常情况下通途会在界面就绪后主动分离控制台（见 detachConsole），因此这条路径
// 只在「窗口没能创建、仍挂在终端里」或「--no-tray」时兜底。
func installConsoleCloseHandler(fn func()) {
	consoleClose = fn
	cb := syscall.NewCallback(func(ctrlType uint32) uintptr {
		if consoleCtrlIsClose(ctrlType) {
			if consoleClose != nil {
				consoleClose()
			}
			// 给主 goroutine 时间完成还原；Windows 在关闭事件上会等待数秒。
			time.Sleep(3 * time.Second)
			return 1 // 已处理
		}
		return 0 // 其余事件（Ctrl+C / Ctrl+Break）交给默认处理
	})
	procSetConsoleCtrlHandler.Call(cb, 1)
}

// consoleCtrlIsClose 判断控制台事件是否属于「关闭/注销/关机」类，
// 需要在进程消亡前优雅还原系统状态。
func consoleCtrlIsClose(ctrlType uint32) bool {
	switch ctrlType {
	case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
		return true
	}
	return false
}
