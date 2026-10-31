package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/update"
)

// Version 是发行版本号（语义化版本 x.y.z）。
//
// 版本的唯一权威来源是仓库根目录的 VERSION 文件：改版本号请执行 .\bump.ps1，
// 它会同时改写 VERSION 与这里的默认值，并提醒补 CHANGELOG；
// version_test.go 会保证两者不漂移（两个人都只改一处的话，发出去的版本就对不上了）。
//
// 发布构建还会用 -ldflags "-X main.Version=..." 再覆盖一次（见 build.ps1），
// 所以即使忘了同步这里，exe 里的版本号也是对的。
var Version = "0.7.0"

var flagCheckUpdate bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "显示版本号（--check 联网检查是否有新版本）",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("tongtu", Version)
		if !flagCheckUpdate {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := update.Default().Check(ctx, Version)
		switch {
		case errors.Is(err, update.ErrNoRelease):
			fmt.Println("仓库还没有发布版本")
		case err != nil:
			return fmt.Errorf("检查更新失败: %w", err)
		case res.Newer:
			fmt.Printf("发现新版本 v%s（当前 v%s）\n%s\n", res.Latest, res.Current, res.URL)
		default:
			fmt.Printf("已是最新版本（v%s）\n", res.Latest)
		}
		return nil
	},
}

func init() {
	versionCmd.Flags().BoolVar(&flagCheckUpdate, "check", false,
		"联网检查 GitHub 上是否有新版本（只检查，不下载）")
	rootCmd.AddCommand(versionCmd)
}
