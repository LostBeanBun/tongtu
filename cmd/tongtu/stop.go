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
	"tongtu/internal/sysproxy"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "停止网络加速服务并还原系统代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		// 控制端口上**确认是通途**才发停止指令：别的程序占了同一个端口时不能拿它
		// 当通途使；识别不出的占用者同理（见 control.PortUnknown）——绝不向陌生进程
		// 发 /shutdown。指令没送达时也不再直接放弃：继续走下面的遗留清理
		//（对方还活着时 RecoverStale 会凭状态文件自己收手），最后如实返回失败。
		var sendErr error
		state := control.ProbeControlState(cfg.Control)
		switch state {
		case control.PortTongtu:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := control.Call(ctx, cfg.Control, "/shutdown", http.MethodPost); err != nil {
				// 万一对方恰好在探测与发送之间崩了，遗留清理正该由 stop 兜底。
				sendErr = fmt.Errorf("发送停止指令失败: %w", err)
				fmt.Printf("警告: %v；继续检查遗留状态\n", sendErr)
			} else {
				// 等待对方完成状态还原并退出
				deadline := time.Now().Add(10 * time.Second)
				for control.Running(cfg.Control) && time.Now().Before(deadline) {
					time.Sleep(200 * time.Millisecond)
				}
				fmt.Println("tongtu 已停止，系统代理已还原（若期间系统代理被其他程序接管，则保留对方的设置）")
				cancel()
				return nil
			}
			cancel()
		case control.PortFree:
			// 没在运行：直接走遗留清理
		default:
			fmt.Printf("提示: 控制端口 %s 被其他程序占用，未向其发送停止指令\n", cfg.Control)
		}

		// 未在运行：清理可能遗留的状态（覆盖崩溃/强杀场景）。
		// 但若另一个实例还活着（换了控制端口的第二实例、或本实例的控制服务已挂而进程仍在），
		// 它才是系统代理的负责人——这里收手，不能把它的接管一起清掉。
		if pid, busy := foreignLiveSession(); busy {
			if sendErr != nil {
				fmt.Printf("另一个通途实例（pid=%d）仍在运行，停止指令未送达，已跳过遗留状态清理\n", pid)
			} else {
				fmt.Printf("tongtu 未在本配置的控制端口上运行，但另一个实例（pid=%d）仍在运行，已跳过遗留状态清理\n", pid)
			}
			return sendErr
		}
		log := newLogger(cfg)
		app.RecoverStale(log)
		// 遗留状态已清理干净：撤掉「下次登录还原」项，别在系统里留下常驻命令。
		// 没清完（或仍由别的实例持有）就保留，留给下次登录再兜底。
		if !sysproxy.Active() {
			if err := autostart.ClearRunOnce(); err != nil {
				log.Warn("撤掉断电自愈项失败", "err", err)
			}
		}
		if sendErr != nil {
			fmt.Println("tongtu 可能仍在运行（停止指令未送达），已检查遗留的系统状态")
			return sendErr
		}
		fmt.Println("tongtu 未在运行，已检查并清理遗留的系统状态")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
