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
	"tongtu/internal/mitm"
	"tongtu/internal/rules"
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
			fmt.Printf("提示: 配置已从 v%d 升级到 v%d，旧文件备份为 %s.v%d.bak\n",
				cfg.UpgradedFrom, config.CurrentVersion, config.DefaultPath(), cfg.UpgradedFrom)
		}
		log := newLogger(cfg)
		rs, err := rules.Load(cfg.Services)
		if err != nil {
			return err
		}

		// 单实例：控制端口可达即视为已有实例
		if control.Running(cfg.Control) {
			return fmt.Errorf("tongtu 已在运行（如需重启请先执行 tongtu stop）")
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

		var mitmCA *mitm.CA
		if cfg.MITM.Enabled && !flagNoMITM {
			mitmCA = ensureMITMCert(cfg, log, !flagHidden)
		}
		wantSys := cfg.SystemProxy && !flagNoSysProxy
		wantHosts := cfg.Hosts && !flagNoHosts
		a := app.New(app.Options{
			Config:     cfg,
			Rules:      rs,
			SysProxy:   wantSys,
			HostsMode:  wantHosts,
			MITMCA:     mitmCA,
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

		// 窗口收到注销/关机通知（WM_QUERYENDSESSION）时：先发退出信号，再等主流程
		// 把系统代理与 hosts 还原完（最多等 4 秒），然后才放行会话结束。
		setSessionEndHandler(func() {
			signalQuit()
			select {
			case <-shutdownDone:
			case <-time.After(4 * time.Second):
			}
		})

		cs := control.New(cfg.Control, a, log)
		cs.OnShutdown = signalQuit
		if err := cs.Start(); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = a.Shutdown(ctx)
			return err
		}

		// 崩溃防护第二步：一旦接管了系统代理 / hosts，就拉起监护进程。
		// 托盘里手动打开开关的情况由 watchGuard 轮询补上。
		watchStop := make(chan struct{})
		defer close(watchStop)
		go watchGuard(a, log, watchStop)
		if guardNeeded(a) {
			startGuard(log)
		}

		st := a.Status()
		// 启动摘要：控制台印纯文本（renderSummaryText），窗口里印同一份内容但带颜色，
		// 因此这里只描述「事实」，两种呈现都从 rows 生成，不会出现两处说法不一致。
		title := "通途已启动"
		mitm := modeRow("无SNI反代", cfg.MITM.Enabled, st.MITM, "已启用", "未启用", "未生效（详见日志）")
		if st.MITM && wantHosts && !st.MITMLocal {
			mitm.Mark = markWarn // 本地 :443 没起来时反代只对系统代理路径有效
			mitm.Value = "仅系统代理路径可用（本地 :443 未就绪）"
		}
		rows := []summaryRow{
			{Mark: markInfo, Label: "代理监听", Value: st.Listen},
			modeRow("系统代理", wantSys, st.SystemProxy, "已接管", "未启用", "未接管（接管失败，详见日志）"),
			modeRow("Hosts", wantHosts, st.Hosts, "已生效", "未启用", "未生效（需要以管理员身份运行）"),
			mitm,
		}
		if row, ok := guardRow(guardBreakawayFailed()); ok {
			rows = append(rows, row)
		}
		rows = append(rows, summaryRow{Mark: markInfo, Label: "服务组", Value: strings.Join(st.Services, "、")})
		if cfg.Log.File != "" {
			rows = append(rows, summaryRow{Mark: markInfo, Label: "日志文件", Value: cfg.Log.File})
		}
		fmt.Print(renderSummaryText(title, rows))
		// 同一份摘要也写进窗口（此时窗口可能还没创建，会先缓存、稍后带样式回填）
		if sink := logDisplay(); sink != nil {
			sink.addSummary(title, rows)
		}

		// 窗口顶部状态条：跟随运行状态刷新（还没显示窗口时也照常更新缓存）
		if hasLogWindow {
			setWindowStatus(windowStatusOf(a, cfg))
			go refreshWindowStatus(a, cfg, quit)
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
// 返回 nil 表示本次不启用无 SNI 反代。
func ensureMITMCert(cfg *config.Config, log *slog.Logger, interactive bool) *mitm.CA {
	ca, err := mitm.LoadOrCreate(config.Dir())
	if err != nil {
		log.Error("本地 CA 不可用，本次不启用无 SNI 反代", "err", err)
		return nil
	}
	installed, _ := mitm.Installed(ca)
	if !installed && cfg.MITM.AutoCert && interactive {
		certPath, _ := mitm.CertPaths(config.Dir())
		if askInstallCert(certPath) {
			if err := mitm.Install(ca); err != nil {
				log.Error("安装根证书失败，本次不启用无 SNI 反代", "err", err)
				return nil
			}
			installed = true
			log.Info("已安装本地根证书")
		}
	}
	if !installed {
		log.Warn("未安装本地根证书，本次不启用无 SNI 反代（steamcommunity / raw 仍不可用；执行 tongtu cert install 可手动安装）")
		return nil
	}
	return ca
}

// askInstallCert 询问是否安装本地根证书。
//
// 有控制台就在终端里问（说明文字可以完整看到）；GUI 模式（双击启动）下没有终端，
// 改用系统对话框，避免「配置里开了无 SNI 反代却因为没人回答而静默不生效」。
func askInstallCert(certPath string) bool {
	desc := "首次启用「无 SNI 反代」需要安装本地根证书：\n\n" +
		"  · 仅安装到【当前用户】存储（无需管理员），私钥留在本机\n" +
		"  · 仅对配置列出的域名解密（默认 steamcommunity / raw.githubusercontent），其余流量端到端隧道不解密\n" +
		"  · Firefox 使用自带证书库，需手动导入 ca.crt；卸载：tongtu cert uninstall\n\n" +
		fmt.Sprintf("证书文件：%s", certPath)
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
	switch level {
	case stateDegraded:
		state = "部分生效"
	case stateOff:
		state = "已停止"
	}
	return windowStatus{
		Title: "通途 Tongtu " + Version,
		State: state,
		Level: level,
		Items: []windowStatusItem{
			{Label: "系统代理", On: st.SystemProxy},
			{Label: "Hosts", On: st.Hosts},
			{Label: "无SNI反代", On: st.MITM},
		},
		Detail: []string{
			"服务组 " + strings.Join(st.Services, "、"),
			fmt.Sprintf("活跃隧道 %d", st.Proxy.Active),
			fmt.Sprintf("↓%s ↑%s", humanBytes(st.Proxy.BytesIn), humanBytes(st.Proxy.BytesOut)),
		},
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
