package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/control"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "停止网络加速服务并还原系统代理与 hosts",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		if control.Running(cfg.Control) {
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
			fmt.Println("tongtu 已停止，系统代理与 hosts 已还原")
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
		fmt.Println("tongtu 未在运行，已检查并清理遗留的系统状态")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
