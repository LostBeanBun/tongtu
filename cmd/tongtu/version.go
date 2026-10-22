package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version 是发行版本号（语义化版本 x.y.z）。
//
// 版本的唯一权威来源是仓库根目录的 VERSION 文件：改版本号请执行 .\bump.ps1，
// 它会同时改写 VERSION 与这里的默认值，并提醒补 CHANGELOG；
// version_test.go 会保证两者不漂移（两个人都只改一处的话，发出去的版本就对不上了）。
//
// 发布构建还会用 -ldflags "-X main.Version=..." 再覆盖一次（见 build.ps1），
// 所以即使忘了同步这里，exe 里的版本号也是对的。
var Version = "0.2.0"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "显示版本号",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("tongtu", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
