package tray

import "tongtu/internal/app"

// 本文件把托盘内部的动作导出给窗口界面复用（见 cmd/tongtu 的窗口动作）。
//
// 为什么要共用：桌面窗口与托盘菜单是同一件事的两个入口——「打开配置文件」「服务组开关」
// 在两处必须行为一致（同样的实现、同样的报错文案）。导出几个薄封装，避免复制一份实现后
// 两边慢慢走偏。

// OpenPath 用系统默认程序打开文件或目录（目录用资源管理器打开）。
func OpenPath(path string) error { return openPath(path) }

// OpenInNotepad 用记事本打开文本文件（.yaml 通常没有文件关联，指定记事本更可靠）。
func OpenInNotepad(path string) error { return openInNotepad(path) }

// NotifyError 弹出错误提示框（窗口不可见时的兜底反馈通道）。
func NotifyError(title, msg string) { notifyError(title, msg) }

// ConfirmRestartElevated 询问是否以管理员身份重启。
func ConfirmRestartElevated() bool { return confirmRestartElevated() }

// ToggleService 切换单个加速服务组（至少保留一个）。
func ToggleService(a *app.App, id string) error { return toggleService(a, id) }
