package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/control"
	"tongtu/internal/resolver"
	"tongtu/internal/rules"
)

// defaultTestHosts 覆盖 Steam 与 GitHub 及其主要附属站点。
var defaultTestHosts = []string{
	"steamcommunity.com",
	"www.steamcommunity.com",
	"help.steamcommunity.com",
	"store.steampowered.com",
	"login.steampowered.com",
	"cdn.steamstatic.com",
	"avatars.steamstatic.com",
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"raw.githubusercontent.com",
	"objects.githubusercontent.com",
	"avatars.githubusercontent.com",
	"gist.githubusercontent.com",
	"gist.github.com",
}

var testCmd = &cobra.Command{
	Use:   "test [域名...]",
	Short: "测试 DoH 解析与连通性（验证加速效果）",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		hosts := args
		if len(hosts) == 0 {
			hosts = defaultTestHosts
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

		// 反代名单就是配置里的 mitm.domains（含内置默认值）。
		mitmPatterns := cfg.MITM.Domains

		proxyUp := control.Running(cfg.Listen)
		if proxyUp {
			fmt.Printf("检测到本地代理 %s，额外测试经代理的 HTTPS 访问\n\n", cfg.Listen)
		} else {
			fmt.Println("本地代理未运行，仅测试解析与直连（可先执行 tongtu start 再测）")
			fmt.Println()
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "域名\t解析IP(干净)\tDNS\tTCP:443\t直连HTTPS\t经代理\t无SNI反代")
		for _, h := range hosts {
			testOne(w, res, cfg.Listen, proxyUp, cfg.MITM.Enabled, mitmPatterns, h)
		}
		return w.Flush()
	},
}

func testOne(w *tabwriter.Writer, res *resolver.Resolver, proxyAddr string, proxyUp, mitmOn bool, mitmPatterns []string, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t0 := time.Now()
	// 用 LookupAll：与代理一致（静态/远程优选 + 多端点并查，且不回退系统 DNS，
	// 避免在污染窗口下展示假 IP）。
	ips := res.LookupAll(ctx, host)
	dnsMs := time.Since(t0).Milliseconds()
	if len(ips) == 0 {
		fmt.Fprintf(w, "%s\t-\t失败\t-\t-\t-\t%s\n", host, mitmMark(mitmOn, mitmPatterns, host))
		return
	}
	ip := ips[0]
	ipStr := ip.String()
	if len(ips) > 1 {
		ipStr = fmt.Sprintf("%s +%d", ip, len(ips)-1)
	}

	tcpStr := "不可达"
	t1 := time.Now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
	if err == nil {
		tcpStr = fmt.Sprintf("%dms", time.Since(t1).Milliseconds())
		_ = conn.Close()
	}
	// 直连（真 SNI）能不能通：反代名单就是为「不通」的域名准备的。
	directStr := "-"
	if s, err := httpsDirect(ctx, host); err != nil {
		directStr = "失败: " + shortErr(err)
	} else {
		directStr = s
	}

	proxyStr := "-"
	if proxyUp {
		if s, err := httpsViaProxy(ctx, proxyAddr, host); err != nil {
			proxyStr = "失败: " + shortErr(err)
		} else {
			proxyStr = s
		}
	}
	fmt.Fprintf(w, "%s\t%s\t%dms\t%s\t%s\t%s\t%s\n", host, ipStr, dnsMs, tcpStr, directStr, proxyStr,
		mitmMark(mitmOn, mitmPatterns, host))
}

// mitmMark 标注该域名会不会走无 SNI 反代：✓ 走、– 直连（配置里没开反代时统一显示「未启用」）。
func mitmMark(mitmOn bool, patterns []string, host string) string {
	if !mitmOn {
		return "未启用"
	}
	if rules.MatchPatterns(host, patterns) {
		return "✓"
	}
	return "–"
}

// httpsDirect 直接（不经代理、带真 SNI）发一次 HTTPS 请求，成功返回 HTTP 状态描述。
func httpsDirect(ctx context.Context, host string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // 关键：绕开系统代理与本地代理，看真实直连结果
			TLSHandshakeTimeout: 8 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}

// httpsViaProxy 通过本地代理发起一次 HTTPS 请求，成功返回 HTTP 状态描述。
func httpsViaProxy(ctx context.Context, proxyAddr, host string) (string, error) {
	u, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(u),
			TLSHandshakeTimeout: 8 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}

func shortErr(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 48 {
		s = s[:48] + "…"
	}
	return s
}

func init() {
	rootCmd.AddCommand(testCmd)
}
