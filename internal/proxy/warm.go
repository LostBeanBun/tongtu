package proxy

// warm.go 实现启动预热：把「首次访问要等几秒」的候选竞速成本，从用户的第一次点击
// 提前到启动阶段在后台完成。
//
// 背景：反代出站要现查干净 DoH、再对多个候选 IP 逐个做 TCP + 无 SNI TLS 探测，
// 冷状态下第一次访问动辄 2–5 秒。预热复用与真实请求完全相同的候选来源与优选缓存
// （见 mitm.go 的 dialTLSNoSNI），因此预热结果对真实流量直接生效——第一个请求
// 命中 goodIP 快路径，不再竞速。

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Warm 在后台预热一组主机名（best-effort：失败只记日志，不影响服务）：
//   - 反代名单内的主机：走与真实请求相同的候选探测，成功后写入优选缓存；
//   - 其余（白名单隧道）：只预热干净解析缓存，省掉首次请求的 DoH 往返。
//
// hosts 为空或 ctx 取消时立即返回。调用方应放进 goroutine，并传入随服务退出而取消的 ctx。
func (p *Proxy) Warm(ctx context.Context, hosts []string, workers int) {
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		// 先看一次 ctx：select 在「两个 case 都就绪」时是随机挑的，
		// 已取消的预热不该因为 sem 恰好有空位而漏放一个 goroutine 出去。
		if ctx.Err() != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			p.warmOne(ctx, h)
		}(h)
	}
	wg.Wait()
}

// warmOne 预热单个主机名。整体超时 15 秒，避免个别坏候选拖住整个预热。
func (p *Proxy) warmOne(ctx context.Context, host string) {
	if ctx.Err() != nil {
		return // 预热期间服务已经在退出：不发任何请求
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if p.mitmMatch(host) {
		if _, ok := p.goodIP(host); ok {
			return // 已有优选，无需重复
		}
		candidates := p.mitmCandidates(ctx, host)
		if len(candidates) == 0 {
			p.log.Debug("预热：反代域名无干净候选", "host", host)
			return
		}
		port := p.warmPort
		if port == "" {
			port = "443"
		}
		conn, ip, err := dialRace(ctx, candidates, func(cctx context.Context, ip net.IP) (net.Conn, error) {
			return p.dialOneNoSNI(cctx, "tcp", port, ip)
		})
		if err != nil {
			p.log.Debug("预热：反代候选全部失败", "host", host, "candidates", len(candidates), "err", err)
			return
		}
		_ = conn.Close()
		p.rememberGood(host, ip)
		p.log.Info("预热完成（无 SNI 反代优选）", "host", host, "ip", ip)
		return
	}

	// 白名单隧道：预热干净解析缓存（LookupAll 带缓存，真实请求随后直接命中）
	if ips := p.whitelistIPs(ctx, host); len(ips) > 0 {
		p.log.Debug("预热完成（干净解析）", "host", host, "ips", len(ips))
	} else {
		p.log.Debug("预热：无干净解析结果", "host", host)
	}
}
