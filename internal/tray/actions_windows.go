//go:build windows

package tray

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"tongtu/internal/mitm"
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

// openLocation 在资源管理器里打开文件所在目录并选中它。
//
// 比「只打开目录」更有用：用户点「打开日志文件」是想看那一个文件，
// 而不是在一个塞满 ca.crt / config.yaml 的目录里自己找。
func openLocation(path string) error {
	cmd := exec.Command("explorer", "/select,"+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// ensureCertForMITM 确保无 SNI 反代所需的根证书已安装（未装时先问再装）。
// 返回 false 表示用户放弃安装——此时不该切换开关。
//
// 为什么放在托盘这一层：窗口与托盘是两个入口，同一个前置条件必须由同一段代码把住，
// 否则「窗口里能用、托盘里报错」这种不一致迟早会出现。
func ensureCertForMITM(dir string) (bool, error) {
	const (
		mbYesNo         = 0x4
		mbIconQuestion  = 0x20
		mbSetForeground = 0x10000
		mbTopMost       = 0x40000
	)
	return mitm.EnsureInstalled(dir, func(prompt string) bool {
		return messageBoxYesNo("通途 Tongtu — 安装根证书", prompt+"\n\n是否现在安装？",
			mbYesNo|mbIconQuestion|mbSetForeground|mbTopMost)
	})
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
