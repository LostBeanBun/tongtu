package main

// 窗口界面能触发的动作与查询，由 start.go 注入（见 setWindowActionsHook）。
//
// 为什么走「注入」而不是让窗口直接拿 *app.App：窗口只负责「用户点了什么」，
// 具体怎么做（切系统代理、写 hosts、提权重启…）留在装配层，两边各自可测。
// 非 Windows 平台没有窗口，这些类型依然要存在（start.go 会用到）。

// 自绘控件的动作 id。
// 命中判定、悬浮说明与动作分发共用这一组常量，避免在三处写错同一个字符串。
const (
	uiIDSys      = "sys"      // 系统代理开关
	uiIDHosts    = "hosts"    // Hosts 开关
	uiIDMITM     = "mitm"     // 无 SNI 反代开关
	uiIDServices = "services" // 服务组菜单
	uiIDConfig   = "config"   // 打开配置文件
	uiIDClear    = "clear"    // 清空日志
	uiIDQuit     = "quit"     // 退出程序
	uiIDOnlyWarn = "onlywarn" // 只看警告
)

// uiService 是一个可选的加速服务组（窗口里的「服务组」菜单用）。
type uiService struct {
	ID   string
	Name string
	On   bool
}

// windowActions 是窗口界面需要的外部动作快照。
type windowActions struct {
	// ToggleMode 切换单项加速模式（id 取 uiIDSys / uiIDHosts / uiIDMITM）。
	ToggleMode func(id string, on bool) error
	// SetServices 直接设置启用的服务组（至少保留一个）。
	SetServices func(ids []string) error
	// Services 返回全部服务组及其当前开关（用于「服务组」菜单）。
	Services func() []uiService
	// OpenConfig 用记事本打开配置文件。
	OpenConfig func() error
	// Quit 退出程序（停止加速并还原系统设置）。
	Quit func()
	// Refresh 立刻回读一次真实状态（切换开关后不必等 3 秒轮询）。
	Refresh func()
}

var windowActionsHook func() windowActions

// setWindowActionsHook 注册窗口动作提供者（只由 start.go 调用一次）。
func setWindowActionsHook(fn func() windowActions) { windowActionsHook = fn }

// currentWindowActions 取一份动作快照；未注册时返回零值，调用方需判空。
func currentWindowActions() windowActions {
	if windowActionsHook == nil {
		return windowActions{}
	}
	return windowActionsHook()
}
