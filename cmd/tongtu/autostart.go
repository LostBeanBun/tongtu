package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"tongtu/internal/autostart"
)

var flagAutostartElevated bool

var autostartCmd = &cobra.Command{
	Use:   "autostart",
	Short: "管理开机自启",
	Long: `管理开机自启。

默认使用 HKCU Run 键（无需管理员），登录时以当前用户权限启动，仅系统代理模式可用。
加 --elevated 则创建「最高权限」计划任务（需以管理员身份运行），
登录时自动以管理员权限启动，hosts 模式亦可用，且不会弹出 UAC。`,
}

var autostartOnCmd = &cobra.Command{
	Use:   "on",
	Short: "开启开机自启",
	RunE: func(cmd *cobra.Command, args []string) error {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("无法确定程序路径: %w", err)
		}
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		cmdLine := autostart.CommandLine(exe)

		if flagAutostartElevated {
			if !autostart.IsAdmin() {
				return fmt.Errorf("创建「最高权限」自启任务需要管理员权限，请以管理员身份运行本命令")
			}
			if err := autostart.EnableTask(cmdLine); err != nil {
				return err
			}
			_ = autostart.DisableRun() // 避免与 Run 项重复启动
			fmt.Println("已开启开机自启（计划任务 · 最高权限，hosts 模式可用）")
			fmt.Printf("  命令: %s\n", cmdLine)
			return nil
		}

		if err := autostart.EnableRun(cmdLine); err != nil {
			return err
		}
		_ = autostart.DisableTask() // 避免重复启动
		fmt.Println("已开启开机自启（当前用户，登录时启动）")
		fmt.Printf("  命令: %s\n", cmdLine)
		if !autostart.IsAdmin() {
			fmt.Println("  提示: 此方式权限为当前用户，hosts 模式不可用；如需连 Steam 客户端，请以管理员执行: tongtu autostart on --elevated")
		}
		return nil
	},
}

var autostartOffCmd = &cobra.Command{
	Use:   "off",
	Short: "关闭开机自启",
	RunE: func(cmd *cobra.Command, args []string) error {
		var errs []string
		if err := autostart.DisableRun(); err != nil {
			errs = append(errs, err.Error())
		}
		if err := autostart.DisableTask(); err != nil {
			errs = append(errs, err.Error())
		}
		if len(errs) > 0 {
			return fmt.Errorf("关闭开机自启时出错: %s", strings.Join(errs, "; "))
		}
		fmt.Println("已关闭开机自启")
		return nil
	},
}

var autostartStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看开机自启状态",
	RunE: func(cmd *cobra.Command, args []string) error {
		info, err := autostart.Query()
		if err != nil {
			return err
		}
		switch {
		case info.TaskCmd != "":
			fmt.Println("开机自启: 开（计划任务 · 最高权限，hosts 模式可用）")
			fmt.Printf("  命令: %s\n", info.TaskCmd)
		case info.RunCmd != "":
			fmt.Println("开机自启: 开（当前用户）")
			fmt.Printf("  命令: %s\n", info.RunCmd)
			if !autostart.IsAdmin() {
				fmt.Println("  提示: 如需 hosts 模式（Steam 客户端），请以管理员执行: tongtu autostart on --elevated")
			}
		default:
			fmt.Println("开机自启: 关")
		}
		return nil
	},
}

func init() {
	autostartOnCmd.Flags().BoolVar(&flagAutostartElevated, "elevated", false, "以最高权限创建计划任务（需管理员；hosts 模式可用）")
	autostartCmd.AddCommand(autostartOnCmd, autostartOffCmd, autostartStatusCmd)
	rootCmd.AddCommand(autostartCmd)
}
