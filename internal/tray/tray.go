// Package tray 提供系统托盘图标与快捷开关菜单。
//
// 托盘右键菜单承载全部可配置项（加速模式、服务组、开机自启等），
// 每一项都可以即时开关，并且改动会写回 config.yaml。
//
// 并发约定：systray 的菜单项不是并发安全的，因此**所有**对菜单项的读写
// （Check/Uncheck/Checked）都只在 clickLoop 这一个 goroutine 里进行；
// 定时刷新通过 syncReq 通道请求，而不是直接操作菜单项。
package tray

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"fyne.io/systray"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/icon"
)

// errLastService 表示用户试图关闭最后一个加速服务组。
var errLastService = errors.New("至少要保留一个加速服务")

// Run 启动系统托盘（内部 goroutine 阻塞直至 Exit）。
// onQuit 在用户点击「退出」时触发，由调用方负责停止服务。
// ready 在托盘菜单就绪后调用一次（可为 nil）；调用方据此确认「托盘真的可用」，
// 之后才把界面从控制台窗口换成通途自己的窗口，避免既没有界面又没有托盘。
// onWindow 为「显示/隐藏窗口」（可为 nil，表示当前平台没有独立窗口，菜单项不出现）。
//
// 注意：fyne.io/systray 的消息循环（创建托盘窗口并 GetMessage）必须始终运行在同一个
// 操作系统线程上，否则 Windows 收不到鼠标事件——表现为「托盘图标可见，但左右键无反应」。
// 这里用 LockOSThread 把本 goroutine 钉在其线程上，保证消息循环不会因 Go 调度迁移线程。
func Run(a *app.App, onQuit func(), ready func(), onWindow func() error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	done := make(chan struct{})
	systray.Run(func() {
		onReady(a, onQuit, done, onWindow)
		if ready != nil {
			ready()
		}
	}, func() {
		close(done)
	})
}

// Exit 请求托盘退出（未启动托盘时调用是安全的 no-op）。
func Exit() {
	systray.Quit()
}

// menu 汇总全部菜单项，便于在单一 goroutine 中统一读写。
type menu struct {
	sys      *systray.MenuItem
	hosts    *systray.MenuItem
	mitm     *systray.MenuItem
	steam    *systray.MenuItem
	github   *systray.MenuItem
	auto     *systray.MenuItem
	autoElev *systray.MenuItem
	window   *systray.MenuItem
	openCfg  *systray.MenuItem
	openDir  *systray.MenuItem
	quit     *systray.MenuItem
}

func onReady(a *app.App, onQuit func(), done chan struct{}, onWindow func() error) {
	systray.SetIcon(icon.ICO)
	systray.SetTitle("通途")
	systray.SetTooltip("通途（Tongtu）· 网络加速（Steam / GitHub）")

	title := systray.AddMenuItem("通途 网络加速", "右键开关；改动会保存到 config.yaml")
	title.Disable()
	systray.AddSeparator()

	m := &menu{
		// —— 加速模式 ——
		sys:   systray.AddMenuItemCheckbox("系统代理模式", "接管浏览器与系统流量（无需管理员）", false),
		hosts: systray.AddMenuItemCheckbox("Hosts 模式", "覆盖 hosts 解析，供 Steam 客户端使用（需管理员）", false),
		mitm:  systray.AddMenuItemCheckbox("无 SNI 反代", "本地解密被封域名，出站剥离 SNI（需先安装根证书）", false),
	}
	systray.AddSeparator()

	// —— 加速服务组 ——
	m.steam = systray.AddMenuItemCheckbox("加速 Steam", "steamcommunity.com、store.steampowered.com 等", false)
	m.github = systray.AddMenuItemCheckbox("加速 GitHub", "github.com、raw.githubusercontent.com 等", false)
	systray.AddSeparator()

	// —— 开机自启 ——
	m.auto = systray.AddMenuItemCheckbox("开机自启", "登录时以当前用户身份自动启动（无需管理员）", false)
	m.autoElev = systray.AddMenuItemCheckbox("开机自启（最高权限）", "登录时以管理员身份启动，hosts 模式亦可用且无 UAC 弹窗", false)
	if !autostart.IsAdmin() {
		m.autoElev.Disable() // 非管理员无法创建“最高权限”计划任务
	}
	systray.AddSeparator()

	// —— 其它 ——
	if onWindow != nil {
		m.window = systray.AddMenuItem("显示/隐藏窗口", "显示或隐藏运行日志窗口（点窗口的 × 只隐藏窗口，不会退出程序）")
		// 左键点托盘图标也能把窗口调出来（更符合托盘应用的习惯；右键仍是菜单）
		systray.SetOnTapped(func() {
			if err := onWindow(); err != nil {
				// 用 goroutine 弹框，避免模态框卡住托盘的消息循环
				go notifyError("通途", "显示/隐藏窗口失败：\n"+err.Error())
			}
		})
	}
	m.openCfg = systray.AddMenuItem("打开配置文件", "用记事本打开 config.yaml")
	m.openDir = systray.AddMenuItem("打开配置目录", "在资源管理器中打开数据目录")
	systray.AddSeparator()
	m.quit = systray.AddMenuItem("退出", "停止加速并还原系统代理与 hosts")

	setCheck := func(item *systray.MenuItem, on bool) {
		if on {
			item.Check()
		} else {
			item.Uncheck()
		}
	}

	// syncState 同步「运行状态」相关勾选。轮询是为了反映控制 API 的变更，
	// 以及启动时接管失败的实际情况（例如未以管理员身份运行时 hosts 未生效）。
	syncState := func() {
		st := a.Status()
		setCheck(m.sys, st.SystemProxy)
		setCheck(m.hosts, st.Hosts)
		setCheck(m.mitm, st.MITM)
		setCheck(m.steam, contains(st.Services, "steam"))
		setCheck(m.github, contains(st.Services, "github"))
	}

	// refreshAuto 同步「开机自启」勾选。schtasks 每次查询都要新建进程，开销较大，
	// 因此只在启动时与用户切换后刷新，不放进轮询。
	refreshAuto := func() {
		info, _ := autostart.Query()
		setCheck(m.auto, info.RunCmd != "")
		setCheck(m.autoElev, info.TaskCmd != "")
	}

	// fail 在独立 goroutine 里弹提示框，避免模态框阻塞菜单事件循环。
	fail := func(msg string) { go notifyError("通途", msg) }

	syncState()
	refreshAuto()

	// syncReq 让定时器请求刷新，而不直接碰菜单项。
	syncReq := make(chan struct{}, 1)
	requestSync := func() {
		select {
		case syncReq <- struct{}{}:
		default:
		}
	}

	go clickLoop(a, m, done, onQuit, onWindow, syncReq, syncState, refreshAuto, fail)

	// 周期同步勾选状态（反映控制 API 的变更与启动失败的实际情况）
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				requestSync()
			}
		}
	}()
}

// clickLoop 是唯一读写菜单项的 goroutine。
func clickLoop(a *app.App, m *menu, done chan struct{}, onQuit func(), onWindow func() error,
	syncReq chan struct{}, syncState, refreshAuto func(), fail func(string)) {
	// 没有「显示/隐藏窗口」菜单项时 windowCh 为 nil，对应分支永不触发。
	var windowCh chan struct{}
	if m.window != nil {
		windowCh = m.window.ClickedCh
	}
	for {
		select {
		case <-done:
			return

		case <-syncReq:
			syncState()

		case <-m.sys.ClickedCh:
			st := a.Status()
			if err := a.SetMode(!st.SystemProxy, st.Hosts); err != nil {
				fail("切换系统代理模式失败：\n" + err.Error())
			}
			syncState()

		case <-m.hosts.ClickedCh:
			st := a.Status()
			if err := a.SetMode(st.SystemProxy, !st.Hosts); err != nil {
				fail("切换 Hosts 模式失败：\n" + err.Error())
			}
			syncState()

		case <-m.mitm.ClickedCh:
			st := a.Status()
			if err := a.SetMITM(!st.MITM); err != nil {
				fail("切换无 SNI 反代失败：\n" + err.Error())
			}
			syncState()

		case <-m.steam.ClickedCh:
			if err := toggleService(a, "steam"); err != nil {
				fail("切换加速服务失败：\n" + err.Error())
			}
			syncState()

		case <-m.github.ClickedCh:
			if err := toggleService(a, "github"); err != nil {
				fail("切换加速服务失败：\n" + err.Error())
			}
			syncState()

		case <-m.auto.ClickedCh:
			if m.auto.Checked() {
				if err := autostart.DisableRun(); err != nil {
					fail("关闭开机自启失败：\n" + err.Error())
				}
			} else if err := enableAutostart(false); err != nil {
				fail("开启开机自启失败：\n" + err.Error())
			}
			refreshAuto()

		case <-m.autoElev.ClickedCh:
			if m.autoElev.Checked() {
				if err := autostart.DisableTask(); err != nil {
					fail("删除开机自启计划任务失败：\n" + err.Error())
				}
			} else if err := enableAutostart(true); err != nil {
				fail("创建开机自启计划任务失败：\n" + err.Error())
			}
			refreshAuto()

		case <-windowCh:
			if err := onWindow(); err != nil {
				fail("显示/隐藏窗口失败：\n" + err.Error())
			}

		case <-m.openCfg.ClickedCh:
			if err := openInNotepad(a.ConfigPath()); err != nil {
				fail("打开配置文件失败：\n" + err.Error() + "\n路径：" + a.ConfigPath())
			}

		case <-m.openDir.ClickedCh:
			if err := openPath(a.DataDir()); err != nil {
				fail("打开配置目录失败：\n" + err.Error() + "\n路径：" + a.DataDir())
			}

		case <-m.quit.ClickedCh:
			onQuit()
			return
		}
	}
}

// toggleService 切换单个加速服务组；不允许把服务全部关掉。
func toggleService(a *app.App, id string) error {
	st := a.Status()
	var next []string
	if contains(st.Services, id) {
		for _, s := range st.Services {
			if s != id {
				next = append(next, s)
			}
		}
		if len(next) == 0 {
			return errLastService
		}
	} else {
		next = append(append([]string(nil), st.Services...), id)
	}
	return a.SetServices(next)
}

// enableAutostart 写入开机自启：elevated 为 false 用 HKCU Run 键，true 用最高权限计划任务。
// 与 CLI 一致：两种方式互斥，开启一种会移除另一种，避免登录时被启动两次。
func enableAutostart(elevated bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	cmdLine := autostart.CommandLine(exe)
	if elevated {
		if err := autostart.EnableTask(cmdLine); err != nil {
			return err
		}
		return autostart.DisableRun()
	}
	if err := autostart.EnableRun(cmdLine); err != nil {
		return err
	}
	return autostart.DisableTask()
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
