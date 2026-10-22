package main

// 窗口界面能触发的动作与查询，由 start.go 注入（见 setWindowActionsHook）。
//
// 为什么走「注入」而不是让窗口直接拿 *app.App：窗口只负责「用户点了什么」，
// 具体怎么做（切系统代理、写 hosts、提权重启…）留在装配层，两边各自可测。
// 非 Windows 平台没有窗口，这些类型依然要存在（start.go 会用到）。

import (
	"context"

	"tongtu/internal/selfcheck"
)

// 自绘控件的动作 id。
// 命中判定、悬浮说明与动作分发共用这一组常量，避免在三处写错同一个字符串。
const (
	uiIDSys      = "sys"      // 系统代理开关
	uiIDHosts    = "hosts"    // Hosts 开关
	uiIDMITM     = "mitm"     // 无 SNI 反代开关
	uiIDServices = "services" // 服务组菜单
	uiIDSelfTest = "selftest" // 端到端自检
	uiIDTodo     = "todo"     // 待办菜单（只在有需要处理的事时出现）
	uiIDLog      = "log"      // 日志菜单
	uiIDConfig   = "config"   // 打开配置文件
	uiIDClear    = "clear"    // 清空窗口日志（日志菜单里的一项）
	uiIDOpenLog  = "openlog"  // 打开日志文件（日志菜单里的一项）
	uiIDDiag     = "diag"     // 复制诊断信息（日志菜单里的一项）
	uiIDQuit     = "quit"     // 退出程序
	uiIDOnlyWarn = "onlywarn" // 只看警告

	// 待办项的动作 id（由「待办」菜单触发）。
	uiIDTodoCert = "todocert" // 安装本地根证书
	uiIDTodoElev = "todoelev" // 以管理员身份重启
	uiIDTodoAuto = "todoauto" // 开启开机自启
	uiIDTodoTake = "todotake" // 重新接管系统代理
)

// uiService 是一个可选的加速服务组（窗口里的「服务组」菜单用）。
type uiService struct {
	ID   string
	Name string
	On   bool
}

// uiTodoItem 是「待办」菜单里的一条：一件需要用户处理的事。
// Label 自带「为什么要做」，因此菜单一眼能读懂（Windows 菜单没法显示悬浮说明）。
type uiTodoItem struct {
	ID    string
	Label string
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

	// SelfTest 跑一次端到端自检（约 10 秒；窗口在后台 goroutine 上调用它）。
	// ctx 取消时应尽快收手（见 internal/selfcheck）。
	SelfTest func(ctx context.Context) []selfcheck.Row
	// LogPath 返回当前日志文件路径（未启用文件日志时返回空串）。
	LogPath func() string
	// OpenLog 打开日志文件（文件不存在时退回打开数据目录）。
	OpenLog func() error
	// Diagnostics 生成一份可粘贴给他人排查的文本。
	Diagnostics func() string
	// CertReady 报告本地根证书是否已安装（窗口在开关反代前用它做前置检查）。
	CertReady func() bool
	// InstallCert 安装本地根证书到当前用户存储（不需要管理员）。
	InstallCert func() error
	// Todos 返回当前待办项（为空表示没有需要处理的事，界面不显示「待办」按钮）。
	Todos func() []uiTodoItem
	// RunTodo 执行一条待办（id 取 uiIDTodo* 之一）。
	RunTodo func(id string) error
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
