package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/config"
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

		// 检测口径要以「运行中的实例」为准，不能拿配置文件当现实：
		// 启动时端口被占会回退到空闲端口（cfg.Listen 只是意图），运行期间用户
		// 编辑 listen 也不会生效；反代开关与域名表同样以实例回报的运行态为准。
		// 与窗口自检（selfcheck_view.go）同一套兜底：拿不到实例时退回配置。
		listen, mitmOn, patterns := probeTargets(cfg, runningStatus(cfg))

		proxyUp := control.Running(listen)
		if proxyUp {
			fmt.Printf("检测到本地代理 %s，额外测试经代理的 HTTPS 访问\n\n", listen)
		} else {
			fmt.Println("本地代理未运行，仅测试解析与直连（可先执行 tongtu start 再测）")
			fmt.Println()
		}

		p := &selfcheck.Prober{
			Res:          res,
			Listen:       listen,
			ProxyUp:      proxyUp,
			MITMOn:       mitmOn,
			MITMPatterns: patterns,
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

// runningStatus 查询运行中实例的状态快照。
//
// 实例不在运行、控制端口被别的程序占用、通信失败或响应不是状态 JSON 时返回 nil
// （调用方退回配置口径）——test 命令本身不该因为拿不到状态而失败。
func runningStatus(cfg *config.Config) *app.Status {
	if control.ProbeControlState(cfg.Control) != control.PortTongtu {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := control.Call(ctx, cfg.Control, "/status", "GET")
	if err != nil {
		return nil
	}
	var st app.Status
	if json.Unmarshal(body, &st) != nil {
		return nil
	}
	return &st
}

// probeTargets 决定「经代理 / 无SNI反代」两列的检测口径（纯函数，见 test_probe_test.go）：
//
//   - 监听地址与反代开关：运行实例回报的实际值优先——端口被占时实例已回退到空闲端口，
//     运行期间编辑配置的 listen 也不会生效，拿 cfg 当现实会把健康的代理报成没在跑
//     （或把陌生占用者当成通途）；拿不到实例时退回配置（此时本来也没得测经代理）。
//   - 反代域名表：始终取生效全集（mitm.domains + 自定义 mitm_domains），
//     只看 cfg.MITM.Domains 会漏掉用户在窗口里加的站点，把它们误报成未加速。
func probeTargets(cfg *config.Config, st *app.Status) (listen string, mitmOn bool, patterns []string) {
	listen, mitmOn = cfg.Listen, cfg.MITM.Enabled
	if st != nil && st.Listen != "" {
		listen, mitmOn = st.Listen, st.MITM
	}
	return listen, mitmOn, cfg.MITMPatterns()
}
