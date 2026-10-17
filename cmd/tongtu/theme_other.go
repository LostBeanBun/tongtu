//go:build !windows

package main

// enableDarkMenus 非 Windows 平台没有系统原生菜单，空实现。
func enableDarkMenus() {}
