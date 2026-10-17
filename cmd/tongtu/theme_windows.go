//go:build windows

package main

import "golang.org/x/sys/windows"

// enableDarkMenus 让系统绘制的菜单（托盘右键菜单、窗口右键菜单）跟随浅色/深色主题。
//
// Win32 的弹出菜单由系统绘制，没有公开的换色接口，只能整进程声明「允许深色」：
// 用 uxtheme.dll 的未公开导出 SetPreferredAppMode(1=AppMode_AllowDark) 让系统按
// 「设置 → 个性化 → 颜色 → 应用模式」自动选择；FlushMenuThemes 让改动立即生效。
// 这两个导出只有序号、没有名字，所以要按序号取；老系统上没有它们，
// 取不到就静默跳过（菜单保持系统默认外观）。
func enableDarkMenus() {
	dll, err := windows.LoadDLL("uxtheme.dll")
	if err != nil {
		return
	}
	defer dll.Release()

	setMode, err := dll.FindProcByOrdinal(uxthemeSetPreferredAppMode)
	if err != nil {
		return
	}
	const appModeAllowDark = 1
	setMode.Call(appModeAllowDark)

	if flush, err := dll.FindProcByOrdinal(uxthemeFlushMenuThemes); err == nil {
		flush.Call()
	}
}

// uxtheme.dll 中与深色菜单相关的未公开导出序号。
const (
	uxthemeSetPreferredAppMode = 135
	uxthemeFlushMenuThemes     = 136
)
