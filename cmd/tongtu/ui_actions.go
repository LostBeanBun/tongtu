package main

// 窗口界面能触发的动作与查询，由 start.go 注入（见 setWindowActionsHook）。
//
// 为什么走「注入」而不是让窗口直接拿 *app.App：窗口只负责「用户点了什么」，
// 具体怎么做（切系统代理、写 hosts、提权重启…）留在装配层，两边各自可测。
// 非 Windows 平台没有窗口，这些类型依然要存在（start.go 会用到）。

import (
	"context"
	"strings"
	"time"

	"tongtu/internal/selfcheck"
)

// 自绘控件的动作 id。
// 命中判定、悬浮说明与动作分发共用这一组常量，避免在三处写错同一个字符串。
const (
	uiIDSys       = "sys"       // 系统代理开关
	uiIDHosts     = "hosts"     // Hosts 开关
	uiIDMITM      = "mitm"      // 无 SNI 反代开关
	uiIDServices  = "services"  // 服务组菜单
	uiIDSelfTest  = "selftest"  // 端到端自检
	uiIDPause     = "pause"     // 一键暂停 / 恢复全部加速
	uiIDTodo      = "todo"      // 待办菜单（只在有需要处理的事时出现）
	uiIDLog       = "log"       // 日志菜单
	uiIDConfig    = "config"    // 打开配置文件
	uiIDClear     = "clear"     // 清空窗口日志（日志菜单里的一项）
	uiIDOpenLog   = "openlog"   // 打开日志文件（日志菜单里的一项）
	uiIDDiag      = "diag"      // 复制诊断信息（日志菜单里的一项）
	uiIDCopyProxy = "copyproxy" // 复制本地代理地址（日志区右键菜单里的一项）
	uiIDQuit      = "quit"      // 退出程序
	uiIDOnlyWarn  = "onlywarn"  // 只看警告
	uiIDSites     = "sites"     // 自定义站点面板（打开 / 回到日志）
	uiIDSiteAdd   = "siteadd"   // 自定义站点面板里的「添加」

	// 自定义站点面板里每一行的动作 id 前缀（后缀是域名本身）。
	uiSiteDelPrefix  = "sitedel:"
	uiSiteMITMPrefix = "sitemitm:"

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

// uiSite 是「自定义站点」面板里的一行。
//
// MITM 表示这个域名是否同时走无 SNI 反代：面板里每一行都有一个可点的「反代」胶囊，
// 因此打开反代不需要回配置文件改 custom.mitm_domains。
type uiSite struct {
	Pattern string
	MITM    bool
}

// uiSiteDelID / uiSiteMITMID 把域名编进行号：面板每行两个动作（删除 / 反代开关）。
// 用域名做后缀而不是行下标：列表顺序变了也不会点到别的站点上。
func uiSiteDelID(pattern string) string  { return uiSiteDelPrefix + pattern }
func uiSiteMITMID(pattern string) string { return uiSiteMITMPrefix + pattern }

// uiSiteFromID 从行内动作 id 还原域名（不是这类 id 时 ok=false）。
func uiSiteFromID(id, prefix string) (string, bool) {
	if !strings.HasPrefix(id, prefix) {
		return "", false
	}
	p := strings.TrimPrefix(id, prefix)
	if p == "" {
		return "", false
	}
	return p, true
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

	// StopAll 一键暂停：还原系统代理与 hosts、关掉反代，但进程继续运行。
	StopAll func() error
	// PauseFor 定时暂停：d <= 0 表示一直暂停到手动恢复（与 StopAll 等价）。
	// 窗口里点「暂停」选时长走这里，到期由 app 层自动恢复（窗口隐藏时也照样恢复）。
	PauseFor func(d time.Duration) error
	// Resume 把「一键暂停」撤掉的项原样恢复。
	Resume func() error
	// ProxyAddr 返回本地代理地址（如 127.0.0.1:19180），用于「复制代理地址」。
	ProxyAddr func() string

	// —— 自定义站点（窗口「站点」面板）——
	// Sites 返回当前全部自定义站点；AddSite 校验并添加一个（返回规范化后的域名）；
	// RemoveSite 删除一个；SetSiteMITM 切换某个站点是否走无 SNI 反代。
	// 全部由 app 层负责写回配置并让规则立即生效（见 internal/app/custom.go）。
	Sites       func() []uiSite
	AddSite     func(raw string) (string, error)
	RemoveSite  func(pattern string) error
	SetSiteMITM func(pattern string, on bool) error
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
