//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32      = windows.NewLazySystemDLL("user32.dll")
	shell32     = windows.NewLazySystemDLL("shell32.dll")
	procMsgBoxW = user32.NewProc("MessageBoxW")
	// ShellExecuteW 用 "runas" 动词触发 UAC，达到「以管理员身份运行」的效果。
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// openPath 用系统默认程序打开文件或目录（资源管理器打开目录）。
func openPath(path string) error {
	cmd := exec.Command("cmd", "/c", "start", "", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// openInNotepad 用记事本打开文本文件（.yaml 通常没有文件关联，直接指定更可靠）。
func openInNotepad(path string) error {
	return exec.Command("notepad.exe", path).Start()
}

// confirmRestartElevated 询问是否以管理员身份重启。
//
// 这一步必须放在退出之前：点了菜单之后程序会先还原系统设置再退出，
// 万一用户在 UAC 弹窗里点了取消，至少他是知情同意的，不会觉得「程序莫名消失了」。
func confirmRestartElevated() bool {
	const (
		mbYesNo         = 0x4
		mbIconQuestion  = 0x20
		mbSetForeground = 0x10000
		mbTopMost       = 0x40000
	)
	msg := "将以管理员身份重新启动通途：\n\n" +
		"  1. 先还原系统代理与 hosts，当前实例退出\n" +
		"  2. 随后弹出 UAC 授权，请点「是」\n\n" +
		"Hosts 模式需要管理员权限，重启后会立即生效（并可创建「最高权限」开机自启）。\n" +
		"若在 UAC 里选择取消，通途不会自动回来，需手动重新打开。\n\n是否继续？"
	return messageBoxYesNo("通途 Tongtu — 以管理员身份重启", msg, mbYesNo|mbIconQuestion|mbSetForeground|mbTopMost)
}

func messageBoxYesNo(title, msg string, flags uint32) bool {
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return false
	}
	const idYes = 6
	r, _, _ := procMsgBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(flags))
	return int32(r) == idYes
}

// RelaunchElevated 以管理员身份重新启动本程序（触发 UAC）。
//
// 调用时机很关键：必须等当前实例还原完系统设置、控制端口也释放之后再调用
// （见 start.go 退出路径里的调用位置）——否则新实例的单实例检查会发现
// 「tongtu 已在运行」而立刻退出，用户只看到一次 UAC 却什么都没发生。
//
// 用户取消 UAC 时返回 ErrElevationCancelled：这不是故障，不该弹错误框。
func RelaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到自身可执行文件: %w", err)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	// 参数按 CreateProcess 规则转义，保留原来的启动方式（--config / --no-* 等）。
	params, err := windows.UTF16PtrFromString(escapeArgs(os.Args[1:]))
	if err != nil {
		return err
	}
	const swShowNormal = 1
	r, _, callErr := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		0, // lpDirectory：空表示用当前目录
		swShowNormal)
	// ShellExecuteW 的返回值 <= 32 表示失败（这是 Win32 的历史约定）。
	if r <= 32 {
		// ERROR_CANCELLED(1223) / SE_ERR_ACCESSDENIED(5) 都对应「用户没授权」。
		if r == 5 || r == 1223 {
			return ErrElevationCancelled
		}
		if callErr != nil && !errors.Is(callErr, syscall.Errno(0)) {
			return fmt.Errorf("拉起提权实例失败: %v（ShellExecute 返回 %d）", callErr, r)
		}
		return fmt.Errorf("拉起提权实例失败（ShellExecute 返回 %d）", r)
	}
	return nil
}

// escapeArgs 把孩子进程的命令行参数按 Windows 规则拼成一个字符串。
func escapeArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

// notifyError 弹出一个提示框。控制台窗口被隐藏（开机自启）时，这是唯一能让
// 用户看到失败原因的方式，所以菜单里的操作失败都通过它反馈。
func notifyError(title, msg string) {
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return
	}
	const (
		mbOK            = 0x0
		mbIconWarning   = 0x30
		mbSetForeground = 0x10000
		mbTopMost       = 0x40000
	)
	procMsgBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbOK|mbIconWarning|mbSetForeground|mbTopMost))
}
