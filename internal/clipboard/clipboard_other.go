//go:build !windows

package clipboard

import "errors"

// Copy 在非 Windows 平台没有统一的剪贴板接口，直接报不支持。
// 端口留在编译期而不是用空实现：调用方拿到错误后会给用户一句明确的提示，
// 比「说复制成功但其实什么也没发生」好。
func Copy(string) error { return errors.New("当前平台不支持写剪贴板") }
