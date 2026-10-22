package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/control"
)

var flagStatusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看运行状态",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if !control.IsTongtuControl(cfg.Control) {
			return fmt.Errorf("tongtu 未在运行")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, err := control.Call(ctx, cfg.Control, "/status", "GET")
		if err != nil {
			return fmt.Errorf("获取状态失败: %w", err)
		}
		if flagStatusJSON {
			fmt.Println(string(body))
			return nil
		}
		var st app.Status
		if err := json.Unmarshal(body, &st); err != nil {
			return err
		}
		printStatus(&st)
		return nil
	},
}

func printStatus(st *app.Status) {
	fmt.Printf("状态:     %s\n", statusWord(st))
	fmt.Printf("代理监听: %s\n", st.Listen)
	sysTag := onOff(st.SystemProxy)
	if st.ProxyNote != "" {
		sysTag += "（" + st.ProxyNote + "）"
	}
	fmt.Printf("系统代理: %s    Hosts: %s\n", sysTag, onOff(st.Hosts))
	mitmTag := onOff(st.MITM)
	switch {
	case st.MITM && !st.MITMLocal:
		mitmTag += "（仅系统代理路径）"
	case !st.MITM && st.MITMNote != "":
		mitmTag += "（" + st.MITMNote + "）"
	}
	fmt.Printf("无SNI反代: %s\n", mitmTag)
	fmt.Printf("服务组:   %v\n", st.Services)
	fmt.Printf("流量:     请求 %d | 隧道 %d (活跃 %d) | 上传 %s 下载 %s\n",
		st.Proxy.Requests, st.Proxy.Tunnels, st.Proxy.Active,
		humanBytes(st.Proxy.BytesOut), humanBytes(st.Proxy.BytesIn))

	fmt.Println("DNS 端点:")
	for _, h := range st.DNS {
		tag := "[ok]  "
		extra := ""
		switch {
		case h.OK:
			if !h.LastOK.IsZero() {
				extra = " 上次成功 " + h.LastOK.Format("15:04:05")
			}
		case h.LastError == "" && h.LastOK.IsZero():
			tag = "[待测]"
			extra = "（本次启动尚未使用）"
		default:
			tag = "[fail]"
			extra = " " + h.LastError
		}
		fmt.Printf("  %s %s%s\n", tag, h.Endpoint, extra)
	}
}

// statusWord 是 `tongtu status` 第一行的状态词。
//
// 暂停不是「停止」也不是「运行中」：进程还在、随时能恢复，但对外已经什么都没接管。
// 三个状态分开写，脚本里 grep 时也不会把「暂停」误判成「已停止」。
func statusWord(st *app.Status) string {
	switch {
	case st.Paused:
		return "已暂停 (" + formatUptime(st.UptimeSec) + "，可恢复：点窗口「恢复」或托盘「恢复全部加速」)"
	case st.Running:
		return "运行中 (" + formatUptime(st.UptimeSec) + ")"
	}
	return "已停止"
}

func formatUptime(sec int64) string {
	d := time.Duration(sec) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

func init() {
	statusCmd.Flags().BoolVar(&flagStatusJSON, "json", false, "输出原始 JSON")
	rootCmd.AddCommand(statusCmd)
}
