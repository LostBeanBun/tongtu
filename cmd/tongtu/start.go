package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/config"
	"tongtu/internal/control"
	"tongtu/internal/hosts"
	"tongtu/internal/mitm"
	"tongtu/internal/rules"
	"tongtu/internal/selfcheck"
	"tongtu/internal/tray"
)

var (
	flagNoTray     bool
	flagNoSysProxy bool
	flagNoHosts    bool
	flagNoMITM     bool
	flagHidden     bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "启动网络加速服务",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if cfg.UpgradedFrom > 0 {
			fmt.Printf("提示: 配置已从 v%d 升级到 v%d（用户设置已保留），旧文件备份为 %s.v%d.bak\n",
				cfg.UpgradedFrom, config.CurrentVersion, config.DefaultPath(), cfg.UpgradedFrom)
		}
		log := newLogger(cfg)
		rs, err := rules.Load(cfg.Services)
		if err != nil {
			return err
		}

		// 单实例：控制端口上确实跑着通途就退出。
		// 端口被「别的程序」占用时不该误报成「通途已在运行」——直接换一个空闲端口继续，
		// 并把新地址写回配置，使 tongtu status / stop 之后仍能连上本实例。
		occupied, foreign := control.ProbeControl(cfg.Control)
		if occupied && !foreign {
			return fmt.Errorf("tongtu 已在运行（如需重启请先执行 tongtu stop）")
		}
		if foreign {
			if addr, ok := control.FreeLoopbackAddr(cfg.Control); ok {
				log.Warn("控制端口已被其他程序占用，改用空闲端口", "配置", cfg.Control, "实际", addr)
				cfg.Control = addr
				if err := config.Save(configPath(), cfg); err != nil {
					log.Warn("写回控制端口配置失败（tongtu status / stop 可能连不上本实例）", "err", err)
				}
			}
		}

		// 崩溃防护第一步：先占住本轮监护令牌。这样上一轮可能还没退出的监护进程会发现
		// 令牌已经换人，从而不会误清掉本实例马上要建立的状态。
		// 顺序注意：claimGuard 会覆盖 guard.json，所以「是否还有别的实例活着」必须在它之前判断。
		foreignPID, foreignLive := foreignLiveSession()
		claimGuard()
		// 清理上次崩溃/强杀遗留的系统状态。
		// 但如果还有别的实例活着（例如用另一份配置、换了控制端口的第二个实例），系统代理与
		// hosts 归它管：这里必须收手，否则会把它的接管清掉。
		if foreignLive {
			log.Warn("检测到另一个通途实例仍在运行，已跳过遗留状态清理", "pid", foreignPID)
		} else {
			app.RecoverStale(log)
		}

		wantSys := cfg.SystemProxy && !flagNoSysProxy
		wantHosts := cfg.Hosts && !flagNoHosts
		// 崩溃防护第二步：**在真正接管之前**就把监护进程拉起来。
		// 接管发生在 a.Start() 里（写状态文件 + 改注册表 / hosts），如果等启动流程走完
		// 再拉监护，中间这段时间崩溃就没人还原了。此刻可能还什么都没接管：监护进程醒来
		// 走的 RecoverStale 是幂等的，没有待还原状态时它什么都不会做。
		if wantSys || wantHosts {
			// 断电自愈项也要在接管之前登记：万一刚写进注册表就掉电，__recover 是幂等的。
			armRunOnce(log)
			startGuard(log)
		}

		var mitmCA *mitm.CA
		mitmNote := ""
		switch {
		case flagNoMITM:
			if cfg.MITM.Enabled {
				mitmNote = "本次按 --no-mitm 跳过"
			}
		case cfg.MITM.Enabled:
			mitmCA, mitmNote = ensureMITMCert(cfg, log, !flagHidden)
		}
		a := app.New(app.Options{
			Config:     cfg,
			Rules:      rs,
			SysProxy:   wantSys,
			HostsMode:  wantHosts,
			MITMCA:     mitmCA,
			MITMNote:   mitmNote,
			Warmup:     true,
			ConfigPath: flagConfig,
			Log:        log,
		})
		if err := a.Start(); err != nil {
			return err
		}

		quit := make(chan struct{}, 1)
		shutdownDone := make(chan struct{})
		// restartElevated 由托盘菜单写入（见下方 tray.Run 的回调），在主流程退出后读取。
		restartElevated := false
		signalQuit := func() {
			select {
			case quit <- struct{}{}:
			default:
			}
		}
		// 关闭控制台窗口 / 注销 / 关机时也要优雅还原系统状态。
		// 正常路径下界面就绪后我们会主动与控制台分离（见 bringUpUI），
		// 因此这里主要在「没有自绘窗口可依赖」时兜底。
		installConsoleCloseHandler(signalQuit)

		// restartElevatedAsk 是「以管理员身份重启」的统一入口（窗口的待办按钮用它）。
		// 与托盘菜单一致：先确认（说明会短暂断网），再记下意图并请求退出；
		// 真正拉起提权实例在下面主流程退出后（见 tray.RelaunchElevated）。
		restartElevatedAsk := func() bool {
			if !tray.ConfirmRestartElevated() {
				return false
			}
			restartElevated = true
			signalQuit()
			return true
		}

		// 窗口收到注销/关机通知（WM_QUERYENDSESSION）时：先发退出信号，再等主流程
		// 把系统代理与 hosts 还原完（最多等 4 秒），然后才放行会话结束。
		setSessionEndHandler(func() {
			signalQuit()
			select {
			case <-shutdownDone:
			case <-time.After(4 * time.Second):
			}
		})

		// 睡眠/休眠恢复：窗口消息只负责投递，真正的清缓存 / 重写 hosts / 预热
		// 在后台做（RecoverNetwork 自带串行化与防抖）。换网由 app 里的网络指纹轮询兜底。
		setPowerResumeHandler(func(reason string) {
			go func() {
				if err := a.RecoverNetwork(reason); err != nil {
					log.Warn("睡眠恢复未完全成功", "err", err)
				}
			}()
		})

		cs := control.New(cfg.Control, a, log)
		cs.OnShutdown = signalQuit
		// 自绘界面没有控件树：把几何快照挂到 /ui/layout 上，真机脚本按 ID 点按钮。
		cs.Layout = uiLayoutSnapshot
		if err := cs.Start(); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = a.Shutdown(ctx)
			return err
		}

		// 崩溃防护第三步（兜底）：启动时没接管成功、但后来在托盘里打开了开关的情况，
		// 由 watchGuard 轮询补上；已经拉过监护进程时这里就是空操作。
		watchStop := make(chan struct{})
		defer close(watchStop)
		go watchGuard(a, log, watchStop)
		if guardNeeded(a) {
			armRunOnce(log)
			startGuard(log)
		}

		// 改完 config.yaml 保存即生效：只热更新服务组 / 自定义站点 / 反代域名。
		// 放在这里而不是界面就绪之后：--no-tray 或窗口创建失败时同样需要它。
		watchCtx, stopWatch := context.WithCancel(context.Background())
		defer stopWatch()
		go watchConfigFile(watchCtx, a, configPath(), log)

		st := a.Status()
		// 上一轮（提权运行时）留下的 hosts 托管块，如果这一轮没能清掉、也没有接管 hosts，
		// 那些域名会指向一个没人监听的本地端口——必须让用户看见，而不是只躺在日志里。
		leftoverHosts := !st.Hosts && hosts.Managed(hosts.DefaultPath())
		if leftoverHosts {
			log.Warn("检测到上次遗留的 hosts 托管块，需要管理员权限才能清理；相关域名当前不可用",
				"建议", "以管理员身份运行一次 tongtu（或 tongtu stop）让它清理")
		}
		// 启动摘要：控制台印纯文本（renderSummaryText），窗口里印同一份内容但带颜色，
		// 因此这里只描述「事实」，两种呈现都从 rows 生成，不会出现两处说法不一致。
		title := "通途已启动"
		mitm := modeRow("无SNI反代", cfg.MITM.Enabled, st.MITM, "已启用", "未启用", "未生效（详见日志）")
		switch {
		case !st.MITM && mitmNote != "":
			// 未启用就把「为什么」直接写进摘要，别让用户去翻日志猜
			mitm.Mark = markWarn
			mitm.Value = mitmNote
		case st.MITM && wantHosts && !st.MITMLocal:
			mitm.Mark = markWarn // 本地 :443 没起来时反代只对系统代理路径有效
			mitm.Value = "仅系统代理路径可用（本地 :443 未就绪）"
		}
		rows := []summaryRow{
			{Mark: markInfo, Label: "代理监听", Value: st.Listen},
			modeRow("系统代理", wantSys, st.SystemProxy, "已接管", "未启用", "未接管（接管失败，详见日志）"),
			modeRow("Hosts", wantHosts, st.Hosts, "已生效", "未启用", "未生效（需要以管理员身份运行）"),
			mitm,
		}
		if leftoverHosts {
			rows = append(rows, summaryRow{Mark: markWarn, Label: "Hosts 残留",
				Value: "上次写入的托管块没清掉，需以管理员身份运行一次（相关域名当前不可用）"})
		}
		if row, ok := guardRow(guardBreakawayFailed()); ok {
			rows = append(rows, row)
		}
		rows = append(rows, summaryRow{Mark: markInfo, Label: "服务组", Value: strings.Join(st.Services, "、")})
		if path := cfg.LogFilePath(); path != "" {
			rows = append(rows, summaryRow{Mark: markInfo, Label: "日志文件", Value: path})
		}
		fmt.Print(renderSummaryText(title, rows))
		// 同一份摘要也写进窗口（此时窗口可能还没创建，会先缓存、稍后带样式回填）
		if sink := logDisplay(); sink != nil {
			sink.addSummary(title, rows)
		}

		// 窗口顶部状态条：跟随运行状态刷新（还没显示窗口时也照常更新缓存）
		if hasLogWindow {
			setWindowStatus(windowStatusOf(a, cfg))
			wireWindowActions(a, cfg, signalQuit, restartElevatedAsk)
			// 状态刷新与待办清单都用 watchStop（退出时关闭）而不是 quit：
			// quit 是容量 1 的缓冲通道，只够一个接收者拿到信号——多一个 goroutine 抢，
			// 主流程就可能永远等不到退出信号（托盘点了「退出」却还在跑）。
			go refreshWindowStatus(a, cfg, watchStop)
			go runTodoBoard(a, cfg, watchStop)
		}

		if !flagNoTray {
			// 托盘「显示/隐藏窗口」：窗口点 X 只是隐藏，用它（或左键点托盘图标）再调出来。
			var toggleWindow func() error
			if hasLogWindow {
				toggleWindow = toggleLogWindow
			}
			ready := make(chan struct{}, 1)
			// restartElevated 由托盘菜单写入、在主流程退出后读取：
			// 中间隔着 quit 通道的收发，天然有 happens-before，不需要额外加锁。
			go tray.Run(a, signalQuit, func() {
				select {
				case ready <- struct{}{}:
				default:
				}
			}, toggleWindow, func() {
				restartElevated = true
				signalQuit()
			}, func() {
				checkUpdateDialog(log)
			})
			fmt.Println("  托盘图标已就绪（右键可退出并还原系统设置）")
			// 双击启动的实例在托盘就绪后弹出通途自己的窗口；随后与控制台分离，
			// 于是「关掉启动它的终端窗口」不再影响运行（详见 bringUpUI）。
			go bringUpUI(ready, log, !flagHidden)
		} else if hasLogWindow && !flagHidden {
			// --no-tray：没有托盘，窗口是唯一的界面
			if err := showLogWindow(); err != nil {
				log.Warn("无法创建界面窗口，保留控制台窗口以便查看日志", "err", err)
			} else {
				log.Info("窗口已打开：点 × 只隐藏窗口，服务继续运行（退出请执行 tongtu stop）")
			}
		}

		fmt.Println("  按 Ctrl+C 停止")

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
			fmt.Println("\n收到中断信号，正在停止…")
		case <-quit:
			fmt.Println("\n正在停止…")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var errs []error
		if err := a.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		} else {
			// 状态还原成功：顺手回收本轮监护令牌，别在数据目录里留下陈旧文件。
			// 还原失败时故意留着——监护进程还能凭它做兜底还原。
			releaseGuard()
			// 同样撤掉断电自愈项：系统里已经没有需要还原的东西了。
			// （还原失败时不撤，留给下次登录的 __recover 兜底。）
			disarmRunOnce(log)
		}
		if err := cs.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if !flagNoTray {
			tray.Exit()
		}
		// 通知「注销/关机」路径：还原已经完成，可以放行会话结束了。
		close(shutdownDone)
		// 「以管理员身份重启」放在最后一步：此时系统状态已还原、控制端口也已释放，
		// 提权后的新实例才不会因为「tongtu 已在运行」而立刻退出。
		if restartElevated {
			switch err := tray.RelaunchElevated(); {
			case err == nil:
				log.Info("已请求以管理员身份重启通途")
			case errors.Is(err, tray.ErrElevationCancelled):
				log.Info("已取消以管理员身份重启")
			default:
				log.Error("以管理员身份重启失败", "err", err)
				messageBox("通途 Tongtu — 提权重启失败",
					err.Error()+"\n\n请手动用「以管理员身份运行」重新打开通途。", mbOK|mbIconError)
			}
		}
		if len(errs) > 0 {
			return errs[0]
		}
		return nil
	},
}

// bringUpUI 在托盘就绪后亮出界面，然后与控制台分离。
//
// 「分离」是「关掉 cmd / 终端窗口不再结束通途」的关键：只要进程还挂在控制台上，
// 用户关闭那个控制台窗口时系统会发送 CTRL_CLOSE_EVENT，并在处理函数返回后强制结束
// 进程（返回「已处理」也拦不住）。通途以 GUI 子系统编译、双击时本来就没有控制台，
// 这里主要处理「从终端启动」的情形：终端里照常看到启动信息，之后进程脱离出去独自运行。
//
// show 为 false（--hidden，开机自启）时仍然会创建一个隐藏窗口：注销 / 关机时它负责
// 收到 WM_QUERYENDSESSION，让通途有机会把系统代理与 hosts 还原干净。
func bringUpUI(ready <-chan struct{}, log *slog.Logger, show bool) {
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		log.Warn("托盘未就绪（可能有另一个 tongtu 实例在运行）")
	}
	if hasLogWindow {
		if err := prepareLogWindow(); err != nil {
			// 没有自绘窗口可依赖时保留控制台，至少还能看到日志
			log.Warn("无法创建界面窗口，保留控制台窗口以便查看日志", "err", err)
			return
		}
		if show {
			if err := showLogWindow(); err != nil {
				log.Warn("显示界面窗口失败（可从托盘菜单重试）", "err", err)
			}
		}
	}
	if detachConsole() {
		log.Info("已从启动它的终端分离：关闭该终端窗口不再影响通途",
			"退出", "托盘右键 → 退出（或执行 tongtu stop）")
	} else if !consoleAttached() {
		// GUI 子系统启动（双击、开机自启）本来就没有控制台，这里说清楚「关终端不影响」
		log.Info("界面已就绪，通途不依赖终端运行",
			"说明", "关闭终端窗口不影响通途", "退出", "托盘右键 → 退出（或执行 tongtu stop）")
	} else {
		// 仍然挂在控制台上（例如 --no-tray 故意保留终端，或窗口创建失败）
		log.Debug("界面已就绪，但仍连接着启动它的控制台",
			"说明", "该控制台关闭时会一并结束通途（GUI 方式启动不会出现这种情况）")
	}
	log.Debug("界面窗口已就绪", "可见", show)
}

// ensureMITMCert 确保本地根证书可用：加载/生成 CA，未安装时（交互启动下）提示安装。
// interactive 为 false（如开机自启的 --hidden 模式）时不提示，仅记录日志。
// 返回的 note 在「本次不启用反代」时说明原因（用于启动摘要/状态显示），成功时为空串。
func ensureMITMCert(cfg *config.Config, log *slog.Logger, interactive bool) (*mitm.CA, string) {
	ca, err := mitm.LoadOrCreate(config.Dir())
	if err != nil {
		log.Error("本地 CA 不可用，本次不启用无 SNI 反代", "err", err)
		return nil, "根证书加载失败，详见日志"
	}
	installed, _ := mitm.Installed(ca)
	if !installed && cfg.MITM.AutoCert && interactive {
		certPath, _ := mitm.CertPaths(config.Dir())
		if askInstallCert(certPath) {
			if err := mitm.Install(ca); err != nil {
				log.Error("安装根证书失败，本次不启用无 SNI 反代", "err", err)
				return nil, "根证书安装失败，详见日志"
			}
			installed = true
			log.Info("已安装本地根证书")
		}
	}
	if !installed {
		if !interactive {
			// 开机自启等非交互方式下无人可问，这里必须把「怎么办」说清楚
			log.Warn("未安装本地根证书，本次不启用无 SNI 反代（非交互启动不会自动安装）",
				"修复", "执行 tongtu cert install 后重启通途")
		} else {
			log.Warn("未安装本地根证书，本次不启用无 SNI 反代（steamcommunity / raw 仍不可用；执行 tongtu cert install 可手动安装）")
		}
		return nil, "缺根证书，执行 tongtu cert install 后重启"
	}
	return ca, ""
}

// askInstallCert 询问是否安装本地根证书。
//
// 有控制台就在终端里问（说明文字可以完整看到）；GUI 模式（双击启动）下没有终端，
// 改用系统对话框，避免「配置里开了无 SNI 反代却因为没人回答而静默不生效」。
func askInstallCert(certPath string) bool {
	desc := mitm.InstallPrompt(certPath)
	if !consoleAvailable() {
		return messageBox("通途 Tongtu — 安装根证书", desc+"\n\n是否现在安装？",
			mbYesNo|mbIconQuestion) == messageBoxYes
	}
	fmt.Println(desc)
	fmt.Print("是否安装? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// confirmInstallCert 在窗口里询问是否安装本地根证书。
//
// 窗口路径总是 GUI（没有终端可问），因此直接用系统对话框。文案与启动时的询问、
// 托盘的询问共用 mitm.InstallPrompt，三处说的是同一件事。
func confirmInstallCert() bool {
	certPath, _ := mitm.CertPaths(config.Dir())
	return messageBox("通途 Tongtu — 安装根证书",
		mitm.InstallPrompt(certPath)+"\n\n是否现在安装？",
		mbYesNo|mbIconQuestion) == messageBoxYes
}

// openLogInExplorer 在资源管理器里定位日志文件；文件还没生成时退回到打开数据目录。
//
// 两种情况都要给出「东西在哪」这个答案：刚启动就点「打开日志文件」时文件可能还没写，
// 报错说「找不到文件」没有意义。
func openLogInExplorer(cfg *config.Config) error {
	path := cfg.LogFilePath()
	if path == "" {
		return tray.OpenPath(config.Dir())
	}
	if _, err := os.Stat(path); err != nil {
		return tray.OpenPath(config.Dir())
	}
	return tray.OpenLocation(path)
}

func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

// windowStatusOf 生成窗口顶部状态条的内容：一眼看清开关状态与流量。
//
// cfg 与 App 共享同一个配置对象（托盘切换会实时改它），因此「期望开什么」按当前配置判断：
// 期望接管却没接上（例如非管理员下 hosts 写不进去）时状态词变成「部分生效」并用橙色，
// 让界面自己把降级说出来，而不是只写在日志里。
func windowStatusOf(a *app.App, cfg *config.Config) windowStatus {
	st := a.Status()
	level := statusLevel(st.Running,
		modeWish{cfg.SystemProxy, st.SystemProxy},
		modeWish{cfg.Hosts, st.Hosts},
		modeWish{cfg.MITM.Enabled, st.MITM})
	state := "运行中"
	switch {
	case st.Paused:
		// 用户自己按了「暂停」：这不是故障，别报成「部分生效」吓人。
		state, level = "已暂停", stateOff
	case level == stateDegraded:
		state = "部分生效"
	case level == stateOff:
		state = "已停止"
	}
	// 健康检查发现的降级（例如所有 DoH 端点失败）不一定体现在三个开关上，
	// 状态词也要跟着变，不能继续显示「运行中」把问题藏起来。
	if level == stateOK && st.Health.State == app.HealthDegraded {
		level = stateDegraded
		state = "部分生效"
	}
	detail := []string{
		"服务组 " + strings.Join(st.Services, "、"),
		fmt.Sprintf("活跃隧道 %d", st.Proxy.Active),
		fmt.Sprintf("↓%s ↑%s", humanBytes(st.Proxy.BytesIn), humanBytes(st.Proxy.BytesOut)),
	}
	// 运行时长放最后：窗口窄到放不下时先丢它（fitDetail 从右往左砍），服务组与流量更要紧。
	if st.Running {
		detail = append(detail, "已运行 "+humanUptime(st.UptimeSec))
	}
	if st.ProxyNote != "" {
		// 运行期被别的程序顶掉了系统代理：明说出来，别让人以为还好好的
		detail = append(detail, "系统代理"+st.ProxyNote)
	}
	if st.Health.HealCount > 0 {
		detail = append(detail, fmt.Sprintf("自愈 %d 次", st.Health.HealCount))
	}
	if st.Health.State == app.HealthDegraded && st.Health.Reason != "" {
		detail = append(detail, st.Health.Reason)
	}
	return windowStatus{
		Title:  "通途 Tongtu " + Version,
		State:  state,
		Level:  level,
		Paused: st.Paused,
		Items: []windowStatusItem{
			{Label: "系统代理", On: st.SystemProxy, Action: uiIDSys, Tip: "系统代理模式：接管浏览器与系统流量（无需管理员）。点击开关"},
			{Label: "Hosts", On: st.Hosts, Action: uiIDHosts, Tip: "Hosts 模式：覆盖 hosts 解析，供 Steam 客户端等不走代理的程序使用（需管理员）。点击开关"},
			{Label: "无SNI反代", On: st.MITM, Action: uiIDMITM, Tip: "无 SNI 反代：本地解密被 SNI 封锁的域名，出站剥离 SNI（需先安装根证书）。点击开关"},
		},
		Detail: detail,
		// 待办数量只读缓存（见 todo.go）：算它要问证书存储与计划任务，不能塞进 3 秒轮询。
		TodoN: todos.count(),
	}
}

// guardRow 生成「崩溃防护」摘要行：只在监护进程无法脱离作业对象时出现。
// 一切正常时不出现——没消息就是好消息，摘要里不塞无用的行。
func guardRow(breakawayFailed bool) (summaryRow, bool) {
	if !breakawayFailed {
		return summaryRow{}, false
	}
	return summaryRow{
		Mark:  markWarn,
		Label: "崩溃防护",
		Value: "受限（监护进程无法脱离当前启动方式；异常退出要等下次启动才还原）",
	}, true
}

// modeRow 生成「开关类」摘要行：已生效=✓、配置要开但没生效=！、没配置开=·。
// 三种情况都给出明确说法，用户不用去翻日志猜「为什么这里是关的」。
func modeRow(label string, want, have bool, okText, offText, failText string) summaryRow {
	switch {
	case have:
		return summaryRow{Mark: markOK, Label: label, Value: okText}
	case want:
		return summaryRow{Mark: markWarn, Label: label, Value: failText}
	default:
		return summaryRow{Mark: markInfo, Label: label, Value: offText}
	}
}

// refreshWindowStatus 周期性刷新窗口顶部状态条，直到退出信号到来。
func refreshWindowStatus(a *app.App, cfg *config.Config, quit <-chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-quit:
			return
		case <-t.C:
			setWindowStatus(windowStatusOf(a, cfg))
		}
	}
}

// wireWindowActions 把窗口界面上的动作接到 app 上。
//
// 与托盘菜单走同一套 API（app.SetMode / SetMITM / SetServices），因此两处的行为完全一致：
// 同样的开关、同样的失败文案。窗口只负责「用户点了什么」，具体怎么做留在这一层。
//
// restartElevated 用于「待办 → 以管理员身份重启」，返回 false 表示用户在确认框里取消了。
func wireWindowActions(a *app.App, cfg *config.Config, quit func(), restartElevated func() bool) {
	setWindowActionsHook(func() windowActions {
		st := a.Status()
		return windowActions{
			ToggleMode: func(id string, on bool) error {
				// 单项开关要带上另一项的实际状态（SetMode 同时管系统代理与 hosts）
				switch id {
				case uiIDSys:
					return a.SetMode(on, st.Hosts)
				case uiIDHosts:
					return a.SetMode(st.SystemProxy, on)
				case uiIDMITM:
					return a.SetMITM(on)
				}
				return fmt.Errorf("未知开关 %q", id)
			},
			SetServices: func(ids []string) error { return a.SetServices(ids) },
			Services: func() []uiService {
				on := a.Status().Services
				enabled := make(map[string]bool, len(on))
				for _, id := range on {
					enabled[id] = true
				}
				ids := rules.BuiltinIDs()
				out := make([]uiService, 0, len(ids))
				for _, id := range ids {
					out = append(out, uiService{ID: id, Name: rules.Name(id), On: enabled[id]})
				}
				return out
			},
			OpenConfig:  func() error { return tray.OpenInNotepad(a.ConfigPath()) },
			Quit:        quit,
			Refresh:     func() { setWindowStatus(windowStatusOf(a, cfg)) },
			SelfTest:    func(ctx context.Context) []selfcheck.Row { return runSelfCheck(ctx, a, cfg) },
			LogPath:     func() string { return cfg.LogFilePath() },
			OpenLog:     func() error { return openLogInExplorer(cfg) },
			Diagnostics: func() string { return buildDiagnostics(a, cfg) },
			CertReady: func() bool {
				ok, _ := mitm.CertInstalled(a.DataDir())
				return ok
			},
			InstallCert: func() error { return mitm.InstallIfNeeded(a.DataDir()) },
			Todos:       todos.list,
			RunTodo: func(id string) error {
				err := runTodo(a, cfg, id, restartElevated)
				// 处理完立刻回读：按钮上的数字要跟着变，不能等下一次兜底刷新
				todos.refresh(a, cfg)
				return err
			},
			StopAll: func() error {
				if err := a.StopAll(); err != nil {
					return err
				}
				todos.refresh(a, cfg) // 暂停后待办会变（例如「夺回系统代理」不再需要）
				return nil
			},
			Resume: func() error {
				err := a.Resume()
				todos.refresh(a, cfg)
				return err
			},
			ProxyAddr: func() string {
				if st := a.Status(); st.Running {
					return st.Listen
				}
				return ""
			},
		}
	})
}

func init() {
	// 无子命令时（双击 exe / 直接运行 tongtu）默认执行 start
	rootCmd.RunE = startCmd.RunE
	rootCmd.Args = cobra.NoArgs
	rootCmd.AddCommand(startCmd)

	// 启动相关开关挂在根命令的持久标志上：tongtu --no-tray 与 tongtu start --no-tray 均可
	f := rootCmd.PersistentFlags()
	f.BoolVar(&flagNoTray, "no-tray", false, "不显示系统托盘图标")
	f.BoolVar(&flagNoSysProxy, "no-sysproxy", false, "本次启动不接管系统代理")
	f.BoolVar(&flagNoHosts, "no-hosts", false, "本次启动不接管 hosts")
	f.BoolVar(&flagNoMITM, "no-mitm", false, "本次启动不启用无 SNI 反代")
	f.BoolVar(&flagHidden, "hidden", false, "不显示界面窗口（供开机自启使用）")
}
