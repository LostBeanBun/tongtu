package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"tongtu/internal/autostart"
)

var autostartCmd = &cobra.Command{
	Use:   "autostart",
	Short: "管理开机自启",
	Long: `管理开机自启。

使用 HKCU Run 键（无需管理员），登录时以当前用户权限启动。`,
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

		if err := autostart.EnableRun(cmdLine); err != nil {
			return err
		}
		fmt.Println("已开启开机自启（当前用户，登录时启动）")
		fmt.Printf("  命令: %s\n", cmdLine)
		return nil
	},
}

var autostartOffCmd = &cobra.Command{
	Use:   "off",
	Short: "关闭开机自启",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := autostart.DisableRun(); err != nil {
			return fmt.Errorf("关闭开机自启时出错: %w", err)
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
		if info.RunCmd != "" {
			fmt.Println("开机自启: 开（当前用户）")
			fmt.Printf("  命令: %s\n", info.RunCmd)
		} else {
			fmt.Println("开机自启: 关")
		}
		return nil
	},
}

func init() {
	autostartCmd.AddCommand(autostartOnCmd, autostartOffCmd, autostartStatusCmd)
	rootCmd.AddCommand(autostartCmd)
}
