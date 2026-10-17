//go:build windows

package tray

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32      = windows.NewLazySystemDLL("user32.dll")
	procMsgBoxW = user32.NewProc("MessageBoxW")
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
