package main

func main() {
	// 监护进程（__guard）只做「等主进程退出然后还原设置」这一件事：既不需要界面，
	// 也不应该接到父进程的控制台上（否则父进程的控制台被关闭时它也会被结束）。
	if isGuardInvocation() {
		Execute()
		return
	}
	// 让窗口/字体按显示器 DPI 渲染（必须在创建任何窗口之前调用）。
	setDPIAware()
	// 通途以 GUI 子系统编译（双击不出现 cmd 窗口）；从终端运行时这里把
	// 标准输出接到父进程的控制台，保证 CLI 输出与日志仍然可见。
	ensureConsole()
	// 让系统绘制的菜单（托盘右键菜单、窗口右键菜单）跟随浅色/深色主题。
	enableDarkMenus()
	Execute()
}
