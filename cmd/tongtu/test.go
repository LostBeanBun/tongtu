package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/control"
	"tongtu/internal/resolver"
	"tongtu/internal/selfcheck"
)

var testCmd = &cobra.Command{
	Use:   "test [域名...]",
	Short: "测试 DoH 解析与连通性（验证加速效果）",
	Long: `对白名单域名做一次端到端体检：干净解析 → TCP 可达 → 直连 HTTPS → 经本地代理 HTTPS。

不带参数时体检内置的 Steam / GitHub 站点。窗口界面里的「自检」按钮跑的是同一套逻辑，
只是结果写进日志卡片（见「自检」）。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		hosts := args
		if len(hosts) == 0 {
			hosts = selfcheck.DefaultHosts
		}

		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		res := resolver.New(resolver.Config{
			Endpoints:   cfg.DNS.Endpoints,
			Timeout:     cfg.DNSTimeout(),
			CacheMin:    time.Duration(cfg.DNS.CacheMinSec) * time.Second,
			CacheMax:    time.Duration(cfg.DNS.CacheMaxSec) * time.Second,
			RemoteHosts: cfg.DNS.RemoteHosts,
			StaticHosts: cfg.DNS.StaticHosts,
		}, quiet)

		proxyUp := control.Running(cfg.Listen)
		if proxyUp {
			fmt.Printf("检测到本地代理 %s，额外测试经代理的 HTTPS 访问\n\n", cfg.Listen)
		} else {
			fmt.Println("本地代理未运行，仅测试解析与直连（可先执行 tongtu start 再测）")
			fmt.Println()
		}

		p := &selfcheck.Prober{
			Res:          res,
			Listen:       cfg.Listen,
			ProxyUp:      proxyUp,
			MITMOn:       cfg.MITM.Enabled,
			MITMPatterns: cfg.MITM.Domains,
			Timeout:      20 * time.Second,
		}
		// 每个域名最多 20 秒，整体给足余量后收手（而不是让用户无限等）
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rows := p.Run(ctx, hosts)

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "结论\t域名\t解析IP(干净)\tDNS\tTCP:443\t直连HTTPS\t经代理\t无SNI反代")
		for _, r := range rows {
			ip, dns, tcp, direct, proxy := r.IP, "-", "-", r.Direct, r.Proxy
			if !r.DNSOK {
				ip, dns = "-", "失败"
			} else {
				dns, tcp = fmt.Sprintf("%dms", r.DNSMs), r.TCP
			}
			if !r.ProxyTested {
				proxy = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Verdict.Word(), r.Host, ip, dns, tcp, direct, proxy, r.MITM)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println()
		fmt.Println("结论: " + selfcheck.Summary(rows))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(testCmd)
}
