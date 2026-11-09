package app

// 单个域名的端到端体检（窗口「诊断」面板用）。
//
// 与命令行 tongtu test、窗口「自检」共用 internal/selfcheck：这里只负责把
// 「当前实例的解析器与运行时状态」交给 Prober，不新建组件、也不改任何配置。

import (
	"context"
	"time"

	"tongtu/internal/selfcheck"
)

// Probe 对单个域名做一次体检，返回一行结果。
//
// 复用 App 已经在用的解析器：诊断与真实代理看到的是同一份 DNS 缓存/优选 IP，
// 不会出现「诊断说可达、实际却连不上」。ctx 取消时尽快返回。
func (a *App) Probe(ctx context.Context, host string) selfcheck.Row {
	a.mu.Lock()
	res := a.res
	running := a.ln != nil && !a.stopped
	listen := ""
	if a.ln != nil {
		listen = a.ln.Addr().String()
	}
	a.mu.Unlock()

	p := &selfcheck.Prober{
		Res:     res,
		Listen:  listen,
		ProxyUp: running,
		Timeout: 20 * time.Second,
	}
	rows := p.Run(ctx, []string{host})
	if len(rows) == 0 {
		// ctx 在第一次探测前就被取消：返回一个明确失败的行，界面不必特判。
		return selfcheck.Row{Host: host, Verdict: selfcheck.VerdictFail}
	}
	return rows[0]
}
