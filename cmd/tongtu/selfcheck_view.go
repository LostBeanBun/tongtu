package main

// 自检的「界面侧」：把 internal/selfcheck 的结论渲染成窗口里的摘要行与一行反馈。
//
// 逻辑只有一份（internal/selfcheck），命令行与窗口各渲染各的：
// 终端画表格（见 test.go），窗口写进日志卡片（这里）。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/config"
	"tongtu/internal/control"
	"tongtu/internal/resolver"
	"tongtu/internal/selfcheck"
)

// selfCheckTimeout 是自检的整体上限：每个域名最多 20 秒，这里给足余量后收手，
// 不让「点了自检」变成一个没有尽头的等待。
const selfCheckTimeout = 3 * time.Minute

// runSelfCheck 跑一次端到端自检（与 tongtu test 同一套逻辑）。
func runSelfCheck(ctx context.Context, a *app.App, cfg *config.Config) []selfcheck.Row {
	st := a.Status()
	listen := st.Listen
	if listen == "" {
		listen = cfg.Listen
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
	p := &selfcheck.Prober{
		Res:          res,
		Listen:       listen,
		ProxyUp:      control.Running(listen),
		MITMOn:       st.MITM,
		MITMPatterns: cfg.MITM.Domains,
		Timeout:      20 * time.Second,
	}
	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()
	return p.Run(ctx, selfcheck.DefaultHosts)
}

// selfCheckRows 把体检结果转成日志卡片里的摘要行：一个域名一行，失败/未加速的行用警告色。
func selfCheckRows(rows []selfcheck.Row) []summaryRow {
	out := make([]summaryRow, 0, len(rows)+1)
	for _, r := range rows {
		mark := markInfo
		switch r.Verdict {
		case selfcheck.VerdictOK:
			mark = markOK
		default:
			// 「仅直连」与「不通」都要让用户看见：前者说明加速没生效，后者说明站点不可用
			mark = markWarn
		}
		out = append(out, summaryRow{Mark: mark, Label: r.Host, Value: r.Describe()})
	}
	if len(rows) > 0 {
		out = append(out, summaryRow{Mark: markInfo, Label: "合计", Value: selfcheck.Summary(rows)})
	}
	return out
}

// selfCheckToast 生成自检完成后状态条上的那一行反馈。
func selfCheckToast(rows []selfcheck.Row) (string, logStyle) {
	var fail, direct int
	for _, r := range rows {
		switch r.Verdict {
		case selfcheck.VerdictFail:
			fail++
		case selfcheck.VerdictDirectOnly:
			direct++
		}
	}
	switch {
	case len(rows) == 0:
		return "自检没有结果（已中断）", styleWarn
	case fail > 0:
		return fmt.Sprintf("自检完成：%d 个域名不通，详见上方结果", fail), styleWarn
	case direct > 0:
		return fmt.Sprintf("自检完成：%d 个域名没走加速（仅直连可用）", direct), styleWarn
	default:
		return fmt.Sprintf("自检通过：%d 个域名全部可用", len(rows)), styleOK
	}
}
