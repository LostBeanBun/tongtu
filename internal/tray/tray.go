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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/clipboard"
	"tongtu/internal/icon"
)

// errLastService 表示用户试图关闭最后一个加速服务组。
var errLastService = errors.New("至少要保留一个加速服务")

// ErrElevationCancelled 表示用户在 UAC 弹窗里点了取消（不是失败，不需要报错弹框）。
var ErrElevationCancelled = errors.New("用户取消了 UAC 授权")

// Run 启动系统托盘（内部 goroutine 阻塞直至 Exit）。
// onQuit 在用户点击「退出」时触发，由调用方负责停止服务。
// ready 在托盘菜单就绪后调用一次（可为 nil）；调用方据此确认「托盘真的可用」，
// 之后才把界面从控制台窗口换成通途自己的窗口，避免既没有界面又没有托盘。
// onWindow 为「显示/隐藏窗口」（可为 nil，表示当前平台没有独立窗口，菜单项不出现）。
// onRestartElevated 为「以管理员身份重启」（可为 nil）：调用方应当先优雅还原再退出，
// 最后在控制端口释放之后拉起提权实例（见 RelaunchElevated 的说明）。
//
// 注意：fyne.io/systray 的消息循环（创建托盘窗口并 GetMessage）必须始终运行在同一个
// 操作系统线程上，否则 Windows 收不到鼠标事件——表现为「托盘图标可见，但左右键无反应」。
// 这里用 LockOSThread 把本 goroutine 钉在其线程上，保证消息循环不会因 Go 调度迁移线程。
func Run(a *app.App, onQuit func(), ready func(), onWindow func() error, onRestartElevated func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	done := make(chan struct{})
	systray.Run(func() {
		onReady(a, onQuit, done, onWindow, onRestartElevated)
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
	pause    *systray.MenuItem
	sys      *systray.MenuItem
	hosts    *systray.MenuItem
	mitm     *systray.MenuItem
	steam    *systray.MenuItem
	github   *systray.MenuItem
	auto     *systray.MenuItem
	autoElev *systray.MenuItem
	elev     *systray.MenuItem
	window   *systray.MenuItem
	copyAddr *systray.MenuItem
	openCfg  *systray.MenuItem
	openDir  *systray.MenuItem
	quit     *systray.MenuItem
}

func onReady(a *app.App, onQuit func(), done chan struct{}, onWindow func() error, onRestartElevated func()) {
	systray.SetIcon(icon.ICO)
	systray.SetTitle("通途")
	systray.SetTooltip("通途（Tongtu）· 网络加速（Steam / GitHub）")

	title := systray.AddMenuItem("通途 网络加速", "右键开关；改动会保存到 config.yaml")
	title.Disable()
	systray.AddSeparator()

	// 「暂停 / 恢复」放在最上面：一键撤掉全部对外影响，再点一次原样回来。
	// 它和下面的三个开关是两种粒度：开关调单项，这里是「全都关掉 / 全都开回来」。
	pause := systray.AddMenuItem("暂停全部加速", "还原系统代理与 hosts、关掉无 SNI 反代；进程继续运行，再点一次原样恢复")
	systray.AddSeparator()

	m := &menu{
		pause: pause,
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

	// —— 开机自启 / 权限 ——
	m.auto = systray.AddMenuItemCheckbox("开机自启", "登录时以当前用户身份自动启动（无需管理员）", false)
	m.autoElev = systray.AddMenuItemCheckbox("开机自启（最高权限）", "登录时以管理员身份启动，hosts 模式亦可用且无 UAC 弹窗", false)
	m.elev = systray.AddMenuItem("以管理员身份重启", "先还原系统设置并退出，再弹 UAC 以管理员权限重新启动（Hosts 模式随之生效）")
	if autostart.IsAdmin() {
		m.autoElev.Disable() // 非管理员无法创建“最高权限”计划任务
		m.elev.Disable()
		m.elev.SetTitle("以管理员身份重启（当前已是管理员）")
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
	m.copyAddr = systray.AddMenuItem("复制代理地址", "把本地代理地址复制到剪贴板，便于填到别的程序里")
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
	// 顺带刷新托盘悬停提示：不打开窗口也能看到状态与流量。
	pauseTitle := "" // 记住当前标题，只在真正变化时才 SetTitle（避免每 3 秒重建一次菜单）
	syncState := func() {
		st := a.Status()
		setCheck(m.sys, st.SystemProxy)
		setCheck(m.hosts, st.Hosts)
		setCheck(m.mitm, st.MITM)
		setCheck(m.steam, contains(st.Services, "steam"))
		setCheck(m.github, contains(st.Services, "github"))
		want := "暂停全部加速"
		if st.Paused {
			want = "恢复全部加速"
		}
		if want != pauseTitle {
			pauseTitle = want
			m.pause.SetTitle(want)
		}
		systray.SetTooltip(tooltipText(st))
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

	go clickLoop(a, m, done, onQuit, onWindow, onRestartElevated, syncReq, syncState, refreshAuto, fail)

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
	onRestartElevated func(), syncReq chan struct{}, syncState, refreshAuto func(), fail func(string)) {
	// 没有「显示/隐藏窗口」菜单项时 windowCh 为 nil，对应分支永不触发。
	var windowCh chan struct{}
	if m.window != nil {
		windowCh = m.window.ClickedCh
	}
	// 平台不支持提权重启（或调用方没提供回调）时 elevCh 为 nil，分支同样永不触发。
	var elevCh chan struct{}
	if m.elev != nil && !m.elev.Disabled() && onRestartElevated != nil {
		elevCh = m.elev.ClickedCh
	}
	for {
		select {
		case <-done:
			return

		case <-syncReq:
			syncState()

		case <-m.pause.ClickedCh:
			if a.Paused() {
				if err := a.Resume(); err != nil {
					fail("恢复加速失败：\n" + err.Error())
				}
			} else if err := a.StopAll(); err != nil {
				fail("暂停加速失败：\n" + err.Error())
			}
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
			if !st.MITM {
				// 开启前先确认根证书：没有证书的反代只会失败，
				// 而「去终端执行 tongtu cert install」正是 0.2.0 要去掉的门槛。
				ok, err := ensureCertForMITM(a.DataDir())
				if err != nil {
					fail("安装根证书失败：\n" + err.Error())
					syncState()
					continue
				}
				if !ok {
					syncState()
					continue // 用户放弃安装：开关保持原样
				}
			}
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

		case <-elevCh:
			// 先问一次再退出：UAC 被取消时用户至少是知情同意的，不会「点了菜单程序就没了」。
			if !confirmRestartElevated() {
				continue
			}
			onRestartElevated()
			return

		case <-m.copyAddr.ClickedCh:
			st := a.Status()
			if !st.Running || st.Listen == "" {
				fail("本地代理还没启动，暂时没有地址可复制")
				continue
			}
			if err := clipboard.Copy("http://" + st.Listen); err != nil {
				fail("复制代理地址失败：\n" + err.Error())
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

// tooltipText 生成托盘悬停提示：不打开窗口就能看到运行状态与流量。
//
// 只报「实际状态」（接管成功与否），不写「期望状态」——期望由菜单勾选表达；
// 期望却没生效时状态条会用橙色写出「部分生效」，两处各司其职。
func tooltipText(st app.Status) string {
	state := "已停止"
	switch {
	case st.Paused:
		state = "已暂停"
	case st.Running:
		state = "运行中"
	}
	if st.Running && st.UptimeSec > 0 {
		// 托盘提示没有宽度压力，把「跑了多久」放在状态词后面：排查「是不是刚重启过」很有用。
		state += "（已运行 " + humanUptime(st.UptimeSec) + "）"
	}
	return fmt.Sprintf("通途 Tongtu · %s · 系统代理%s · Hosts%s · 反代%s · ↓%s ↑%s",
		state, mark(st.SystemProxy), mark(st.Hosts), mark(st.MITM),
		shortBytes(st.Proxy.BytesIn), shortBytes(st.Proxy.BytesOut))
}

// humanUptime 把运行秒数写成「人话」（与窗口状态条同一套文案，见 cmd/tongtu/window_status.go）。
//
// 两处各留一份是有意的：托盘的提示文案要独立可测，且 cmd 与 internal 之间不该为了
// 一行格式化函数互相依赖。
func humanUptime(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	case sec < 86400:
		h, m := sec/3600, (sec%3600)/60
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	default:
		d, h := sec/86400, (sec%86400)/3600
		if h == 0 {
			return fmt.Sprintf("%d 天", d)
		}
		return fmt.Sprintf("%d 天 %d 小时", d, h)
	}
}

func mark(on bool) string {
	if on {
		return "✓"
	}
	return "✗"
}

// shortBytes 是托盘提示专用的紧凑体积写法（↓1.2M ↑340K）。
// 与状态条里的「1.2 MiB」刻意区分：提示行要短，状态条要严谨。
// 整数不带小数点（340K 而不是 340.0K），因为托盘提示的宽度是硬约束。
func shortBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	suffix := []string{"K", "M", "G", "T"}
	div, i := int64(unit), 0
	for v := n / unit; v >= unit && i < len(suffix)-1; v /= unit {
		div *= unit
		i++
	}
	num := strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/float64(div)), ".0")
	return num + suffix[i]
}
