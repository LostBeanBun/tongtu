package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version 是发行版本号。
var Version = "0.1.0"

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
