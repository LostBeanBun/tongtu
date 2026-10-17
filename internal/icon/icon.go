// Package icon 提供通途的应用图标（托盘与窗口共用同一份 ICO）。
//
// ICO 里含多个尺寸；托盘直接用整份字节（fyne.io/systray 会挑合适尺寸），
// 窗口则用 CreateIconFromResourceEx 选出最合适的一张。
package icon

import _ "embed"

//go:embed icon.ico
var ICO []byte
