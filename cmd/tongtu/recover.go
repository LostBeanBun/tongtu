package main

// 断电 / 蓝屏自愈（RunOnce 兜底）。
//
// 监护进程（guard.go）能在崩溃 / 强杀后立刻还原，但断电、蓝屏、强制关机时没有任何代码
// 有机会执行：机器再打开时，系统代理还指着 127.0.0.1:19180，而通途并没有在跑——
// 表现就是「开机后网页全打不开」。
//
// 于是接管期间在 HKCU\Software\Microsoft\Windows\CurrentVersion\RunOnce 登记一条
//
//	"<exe>" __recover
//
// 下次登录时由 Windows 执行一次：__recover 走 app.RecoverStale 还原系统代理，
// 然后把这个条目清掉。条目由 watchGuard 每轮巡检补写（被系统执行掉、被别的程序清理掉
// 都会补回来），暂停 / 手动关掉接管 / 优雅退出时撤掉——所以它只在「确实有东西需要
// 还原」时才存在，系统里不会留下常驻项。

import (
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
)

const recoverSubcommand = "__recover"

// runOnceArmed 记录本进程是否已经登记过自愈项：只在状态翻转时写日志，避免每轮巡检都刷一行。
var runOnceArmed atomic.Bool

// RunOnce 的读写入口做成变量：单元测试可以注入假实现，不去碰真实注册表
// （与 useFakeActions 的思路一致）。
var (
	runOnceGet   = autostart.RunOnceCommand
	runOnceSet   = autostart.SetRunOnce
	runOnceClear = autostart.ClearRunOnce
)

// armRunOnce 登记「下次登录还原」项。可重复调用：值没变时不碰注册表。
func armRunOnce(log *slog.Logger) {
	self, err := os.Executable()
	if err != nil {
		if log != nil {
			log.Warn("断电自愈：找不到自身可执行文件，异常掉电后将无法自动还原", "err", err)
		}
		return
	}
	want := autostart.RecoverCommand(self)
	if cur, err := runOnceGet(); err == nil && cur == want {
		runOnceArmed.Store(true)
		return
	}
	if err := runOnceSet(want); err != nil {
		if log != nil {
			log.Warn("断电自愈：无法登记重启还原项（异常掉电后需手动启动一次通途才能恢复网络）", "err", err)
		}
		return
	}
	if !runOnceArmed.Swap(true) && log != nil {
		log.Info("已登记断电自愈：万一异常掉电，下次登录会自动还原系统代理")
	}
}

// disarmRunOnce 撤掉自愈项：系统状态已经全部还原，没什么可兜底的了。
// 可重复调用；本来就不存在时什么都不做（不写注册表）。
func disarmRunOnce(log *slog.Logger) {
	cur, err := runOnceGet()
	if err != nil {
		if log != nil {
			log.Warn("断电自愈：读取重启还原项失败", "err", err)
		}
		return
	}
	if cur == "" {
		runOnceArmed.Store(false)
		return
	}
	if err := runOnceClear(); err != nil {
		if log != nil {
			log.Warn("断电自愈：撤掉重启还原项失败", "err", err)
		}
		return
	}
	runOnceArmed.Store(false)
	guardLog("已撤掉断电自愈项（系统状态已还原）")
}

// recoverCmd 是登录时由 RunOnce 调用的隐藏命令：还原上次异常掉电遗留的状态，然后退出。
// 它必须快进快出、不建界面，也不因配置读取失败而放弃还原网络。
var recoverCmd = &cobra.Command{
	Use:    recoverSubcommand,
	Short:  "还原上次异常掉电遗留的系统状态（系统登录时自动调用）",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log := slog.Default()
		if cfg, err := loadConfig(); err == nil {
			log = newLogger(cfg)
		}
		// 掉电时 guard.json 也会留在数据目录里；主人已经不在了就顺手回收。
		recycleStaleGuard()
		app.RecoverStale(log)
		if err := autostart.ClearRunOnce(); err != nil {
			log.Warn("断电自愈：清理重启还原项失败", "err", err)
		}
		log.Info("断电自愈：已检查并还原上次遗留的系统代理")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(recoverCmd)
}
