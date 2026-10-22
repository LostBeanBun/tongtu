package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/control"
	"tongtu/internal/hosts"
	"tongtu/internal/sysproxy"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "停止网络加速服务并还原系统代理与 hosts",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		// 控制端口上确实跑着通途才发停止指令：别的程序占了同一个端口时不能拿它当通途使。
		if control.IsTongtuControl(cfg.Control) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := control.Call(ctx, cfg.Control, "/shutdown", http.MethodPost); err != nil {
				return fmt.Errorf("发送停止指令失败: %w", err)
			}
			// 等待对方完成状态还原并退出
			deadline := time.Now().Add(10 * time.Second)
			for control.Running(cfg.Control) && time.Now().Before(deadline) {
				time.Sleep(200 * time.Millisecond)
			}
			fmt.Println("tongtu 已停止，系统代理与 hosts 已还原（若期间系统代理被其他程序接管，则保留对方的设置）")
			return nil
		}

		// 未在运行：清理可能遗留的状态（覆盖崩溃/强杀场景）。
		// 但若另一个实例还活着（换了控制端口的第二实例、或本实例的控制服务已挂而进程仍在），
		// 它才是系统代理 / hosts 的负责人——这里收手，不能把它的接管一起清掉。
		if pid, busy := foreignLiveSession(); busy {
			fmt.Printf("tongtu 未在本配置的控制端口上运行，但另一个实例（pid=%d）仍在运行，已跳过遗留状态清理\n", pid)
			return nil
		}
		log := newLogger(cfg)
		app.RecoverStale(log)
		// 遗留状态已清理干净：撤掉「下次登录还原」项，别在系统里留下常驻命令。
		// 没清完（例如 hosts 缺管理员权限、或仍由别的实例持有）就保留，留给下次登录再兜底。
		if !sysproxy.Active() && !hosts.Managed(hosts.DefaultPath()) {
			if err := autostart.ClearRunOnce(); err != nil {
				log.Warn("撤掉断电自愈项失败", "err", err)
			}
		}
		fmt.Println("tongtu 未在运行，已检查并清理遗留的系统状态")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
