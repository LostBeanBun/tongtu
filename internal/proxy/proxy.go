// Package proxy 实现按域名白名单分流的本地正向代理（HTTP 转发 + HTTPS CONNECT 隧道）。
package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"tongtu/internal/resolver"
	"tongtu/internal/rules"
)

// Snapshot 是代理的运行统计快照。
type Snapshot struct {
	Requests int64 `json:"requests"`
	Tunnels  int64 `json:"tunnels"` // 累计 CONNECT 隧道数
	Active   int64 `json:"active"`  // 当前活跃隧道数
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

type counters struct {
	requests atomic.Int64
	tunnels  atomic.Int64
	active   atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// Proxy 是一个本地正向代理：
//   - 白名单命中的域名：经 DoH 干净解析后直连（绕过 DNS 污染）
//   - 其余流量：完全直通，行为与直连一致
type Proxy struct {
	// ruleSet 可运行时替换（托盘切换加速服务组），读写走原子指针。
	ruleSet   atomic.Pointer[rules.Set]
	lookup    LookupFunc
	lookupAll LookupAllFunc
	log       *slog.Logger
	dialer    *net.Dialer
	tr        *http.Transport
	rp        *httputil.ReverseProxy
	c         counters

	// allowLoopback 仅供测试：允许候选 IP 为回环地址（注入本地假上游）
	allowLoopback bool
}

// LookupFunc 解析主机名到 IP 列表（生产实现为 resolver.Resolver.Lookup）。
type LookupFunc func(ctx context.Context, host string) ([]net.IP, error)

// LookupAllFunc 并发查询所有 DoH 端点并返回去重后的候选 IP（生产实现为 resolver.LookupAll）。
type LookupAllFunc func(ctx context.Context, host string) []net.IP

// SetLookupAll 注入「多端点并查」解析函数；隧道路径与预热共用。
func (p *Proxy) SetLookupAll(fn LookupAllFunc) { p.lookupAll = fn }

// New 创建代理实例。
func New(rs *rules.Set, res *resolver.Resolver, log *slog.Logger) *Proxy {
	return NewWithLookup(rs, res.Lookup, log)
}

// NewWithLookup 与 New 相同，但允许注入解析函数（便于测试）。
func NewWithLookup(rs *rules.Set, lookup LookupFunc, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.Default()
	}
	p := &Proxy{
		lookup: lookup,
		log:    log,
		dialer: &net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
	p.ruleSet.Store(rs)
	p.tr = &http.Transport{
		DialContext:           p.dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true, // 纯透传，不改写 body
	}
	p.rp = &httputil.ReverseProxy{
		// 用 Rewrite（Director 已废弃）：它还会顺手丢掉客户端自带的
		// Forwarded / X-Forwarded-* 头，转发更「透明」。
		Rewrite: func(pr *httputil.ProxyRequest) {
			if pr.Out.URL.Scheme == "" {
				pr.Out.URL.Scheme = "http"
			}
			pr.Out.Host = pr.Out.URL.Host
			pr.Out.Header.Del("Proxy-Connection")
		},
		Transport:      p.tr,
		ModifyResponse: stripAltSvc,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Debug("HTTP 转发失败", "host", r.Host, "err", err)
			http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	return p
}

// stripAltSvc 删除上游响应里的 Alt-Svc 广告（HTTP/3 提示）。
//
// 为什么要删：本地代理只处理 TCP，浏览器若读到 Alt-Svc 会去尝试 QUIC(UDP:443)，
// 白白多一次注定失败（或绕过代理）的握手。经代理的 HTTPS 看不到响应头（端到端加密），
// 那里由浏览器「走代理时不用 QUIC」兜住。
func stripAltSvc(resp *http.Response) error {
	resp.Header.Del("Alt-Svc")
	return nil
}

// SetRules 运行时替换白名单规则（托盘切换加速服务组时调用）。并发安全。
func (p *Proxy) SetRules(rs *rules.Set) {
	if rs != nil {
		p.ruleSet.Store(rs)
	}
}

// ServeHTTP 实现 http.Handler。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.c.requests.Add(1)
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		if r.Host == "" {
			http.Error(w, "tongtu proxy: 请求必须使用绝对 URI", http.StatusBadRequest)
			return
		}
		// 兼容使用 origin-form 直连代理的客户端
		r.URL.Scheme = "http"
		r.URL.Host = r.Host
	}
	p.rp.ServeHTTP(w, r)
}

// handleConnect 处理 HTTPS 隧道：不解密内容，仅接管上游解析与拨号。
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		http.Error(w, "missing CONNECT target", http.StatusBadRequest)
		return
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	up, err := p.dialContext(r.Context(), "tcp", target)
	if err != nil {
		p.log.Debug("CONNECT 上游拨号失败", "target", target, "err", err)
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = up.Close()
		return
	}

	up = &countingConn{Conn: up, in: &p.c.bytesIn, out: &p.c.bytesOut}
	p.c.tunnels.Add(1)
	p.c.active.Add(1)
	go func() {
		defer p.c.active.Add(-1)
		defer client.Close()
		defer up.Close()
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(up, client)
			_ = up.Close()
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(client, up)
			_ = client.Close()
			done <- struct{}{}
		}()
		<-done // 一侧结束即关闭另一侧，使其拷贝随之退出
		<-done
	}()
}

// dialRace 并发尝试候选 IP（Happy Eyeballs 式）：首个成功者胜出，返回其连接与所用 IP。
// 用于避免坏 IP 逐个超时造成的长等待。fn 负责单个候选的建连（可含 TLS 握手）。
func dialRace(ctx context.Context, ips []net.IP, fn func(context.Context, net.IP) (net.Conn, error)) (net.Conn, net.IP, error) {
	if len(ips) == 0 {
		return nil, nil, fmt.Errorf("无候选 IP")
	}
	if len(ips) == 1 {
		c, err := fn(ctx, ips[0])
		if err != nil {
			return nil, nil, err
		}
		return c, ips[0], nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type res struct {
		conn net.Conn
		ip   net.IP
		err  error
	}
	ch := make(chan res, len(ips))
	for _, ip := range ips {
		go func(ip net.IP) {
			c, err := fn(rctx, ip)
			ch <- res{c, ip, err}
		}(ip)
	}
	var firstErr error
	for idx := 0; idx < len(ips); idx++ {
		r := <-ch
		if r.err == nil {
			cancel()
			// 回收其余仍可能成功建立的连接，避免泄漏
			remaining := len(ips) - idx - 1
			go func(n int) {
				for k := 0; k < n; k++ {
					if rr := <-ch; rr.conn != nil {
						_ = rr.conn.Close()
					}
				}
			}(remaining)
			return r.conn, r.ip, nil
		}
		if firstErr == nil {
			firstErr = r.err
		}
	}
	return nil, nil, firstErr
}

// dialContext 是全流量的统一拨号入口：
// 白名单域名走干净解析（并查多端点候选，逐 IP 快速失败），未命中或解析不可用时回退系统解析。
func (p *Proxy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	rs := p.ruleSet.Load()
	if err == nil && rs != nil && rs.Match(host) {
		ips := p.whitelistIPs(ctx, host)
		if len(ips) > 0 {
			conn, _, derr := dialRace(ctx, ips, func(cctx context.Context, ip net.IP) (net.Conn, error) {
				dctx, cancel := context.WithTimeout(cctx, 8*time.Second)
				defer cancel()
				return p.dialer.DialContext(dctx, network, net.JoinHostPort(ip.String(), port))
			})
			if derr == nil {
				return conn, nil
			}
			p.log.Debug("白名单 IP 直连均失败，回退系统解析", "host", host, "err", derr)
		} else {
			p.log.Debug("干净解析无候选，回退系统解析", "host", host)
		}
	}
	return p.dialer.DialContext(ctx, network, addr)
}

// whitelistIPs 返回白名单域名的干净候选 IP：优先并查多端点（LookupAll），
// 不可用时回退单端点 Lookup（测试注入路径）。
func (p *Proxy) whitelistIPs(ctx context.Context, host string) []net.IP {
	if p.lookupAll != nil {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		ips := p.lookupAll(cctx, host)
		cancel()
		if len(ips) > 0 {
			return ips
		}
	}
	ips, lerr := p.lookup(ctx, host)
	if lerr != nil || len(ips) == 0 {
		return nil
	}
	return ips
}

// Snapshot 返回统计快照。
func (p *Proxy) Snapshot() Snapshot {
	return Snapshot{
		Requests: p.c.requests.Load(),
		Tunnels:  p.c.tunnels.Load(),
		Active:   p.c.active.Load(),
		BytesIn:  p.c.bytesIn.Load(),
		BytesOut: p.c.bytesOut.Load(),
	}
}

// ResetNetwork 丢弃与旧网络绑定的空闲连接。
//
// 睡眠 / 换网后这些连接已经不可用，继续复用只会让第一批请求卡在超时上；
// 关掉它们，后续请求会重新建连（解析缓存由 app 层单独重置）。
func (p *Proxy) ResetNetwork() {
	p.tr.CloseIdleConnections()
}

// countingConn 在转发过程中累计字节数。
type countingConn struct {
	net.Conn
	in, out *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.in.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.out.Add(int64(n))
	}
	return n, err
}
