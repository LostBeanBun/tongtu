package proxy

// 本文件实现「无 SNI 反代」（MITM）：
//   - 浏览器/客户端 → 本地（由本地 CA 签发的证书解密）
//   - 本地 → 目标站点：TLS 握手不携带 SNI（GFW 仅凭 ClientHello SNI 阻断），
//     以 HTTP 请求的 Host 头完成站点路由（实测 steamcommunity.com / raw.githubusercontent.com 可行）
//   - 候选 IP 来自干净 DoH 并查结果（不使用被污染的系统 DNS），TCP+TLS 逐 IP 探测并缓存优选
//
// 仅对配置显式列出的域名启用；其余流量仍走不解密隧道。

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"tongtu/internal/mitm"
)

// LookupAllFunc 并查干净解析结果（无 SNI 拨号的候选 IP 来源）。
type LookupAllFunc func(ctx context.Context, host string) []net.IP

type goodEntry struct {
	ip  net.IP
	exp time.Time
}

// SetLookupAll 注入并查解析（隧道路径的多端点候选来源）。可选。
func (p *Proxy) SetLookupAll(f LookupAllFunc) {
	if f != nil {
		p.lookupAll = f
	}
}

// SetLookupAllUncached 注入无缓存并查解析（优选 IP 探测的候选来源）。可选。
func (p *Proxy) SetLookupAllUncached(f LookupAllFunc) {
	if f != nil {
		p.lookupAllUncached = f
	}
}

// EnableMITM 装配无 SNI 反代：ca 为本地根证书，match 判断 host 是否需要反代。
// 可在 New 之后随时调用；重复调用会整体替换名单与证书（托盘运行时开关用）。
func (p *Proxy) EnableMITM(ca *mitm.CA, match func(host string) bool) {
	// 上游：拨号不带 SNI，靠 Host 头路由
	mtr := &http.Transport{
		DialTLSContext:        p.dialTLSNoSNI,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true, // 纯转发，不改写 body
	}
	mrp := &httputil.ReverseProxy{
		// 用 Rewrite（Director 已废弃）：它还会顺手丢掉客户端自带的
		// Forwarded / X-Forwarded-* 头，转发更「透明」。
		Rewrite: func(pr *httputil.ProxyRequest) {
			// 解密后的请求为 origin-form，补齐出站 URL（scheme=https → 走 DialTLSContext）
			if pr.Out.URL.Scheme == "" {
				pr.Out.URL.Scheme = "https"
			}
			// Out.Host 保持客户端请求的原始 Host：出站靠 Host 头路由（SNI 已剥离）
			if pr.Out.URL.Host == "" {
				pr.Out.URL.Host = pr.Out.Host
			}
			pr.Out.Header.Del("Proxy-Connection")
		},
		Transport:      mtr,
		ModifyResponse: stripAltSvc,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("无 SNI 反代失败", "host", r.Host, "err", err)
			http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	// handler 自带名单校验，不依赖 p 上的字段，避免运行时替换引发竞态
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !match(stripPort(r.Host)) {
			// 防滥用：本地 MITM 只服务配置列出的域名
			http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		mrp.ServeHTTP(w, r)
	})

	p.mitmMu.Lock()
	p.mitmCa, p.matchMITM = ca, match
	p.mtr, p.mrp, p.mitmHandler = mtr, mrp, handler
	p.mitmMu.Unlock()

	p.goodMu.Lock()
	p.good = make(map[string]goodEntry)
	p.goodMu.Unlock()

	p.mitmActive.Store(true)
}

// DisableMITM 热停用无 SNI 反代：之后的 CONNECT 不再本地解密（走普通隧道）。
// 已建立的在途连接不受影响，因此这里只翻标志、保留内部组件。
func (p *Proxy) DisableMITM() {
	p.mitmActive.Store(false)
}

// MITMEnabled 报告无 SNI 反代当前是否启用。
func (p *Proxy) MITMEnabled() bool { return p.mitmActive.Load() }

// mitmMatch 判断 host 是否需要走反代（未启用时恒为 false）。
func (p *Proxy) mitmMatch(host string) bool {
	if !p.mitmActive.Load() {
		return false
	}
	p.mitmMu.RLock()
	match := p.matchMITM
	p.mitmMu.RUnlock()
	return match != nil && match(host)
}

// mitmComponents 取回当前证书与名单的快照（供 TLS 配置使用）。
func (p *Proxy) mitmComponents() (*mitm.CA, func(string) bool) {
	p.mitmMu.RLock()
	defer p.mitmMu.RUnlock()
	return p.mitmCa, p.matchMITM
}

// mitmHandlerSnapshot 取回当前反代处理器（供单连接服务使用）。
func (p *Proxy) mitmHandlerSnapshot() http.Handler {
	p.mitmMu.RLock()
	defer p.mitmMu.RUnlock()
	return p.mitmHandler
}

// handleMITMConnect 处理需要反代的 CONNECT：本地终结 TLS 后按 HTTP 逐请求转发。
func (p *Proxy) handleMITMConnect(w http.ResponseWriter, r *http.Request, target string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, br, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		return
	}
	_, port, _ := net.SplitHostPort(target)
	if port == "" {
		port = "443"
	}
	p.c.tunnels.Add(1)
	p.c.active.Add(1)
	go func() {
		defer p.c.active.Add(-1)
		p.serveMITMStream(client, br.Reader, port)
	}()
}

// serveMITMStream 在已建立（或已 200）的连接上完成服务端 TLS 握手并逐请求转发。
// defaultPort 用于请求 Host 头缺端口时的出站回退（CONNECT 目标端口或 443）。
func (p *Proxy) serveMITMStream(client net.Conn, br *bufio.Reader, defaultPort string) {
	defer client.Close()

	// 客户端侧字节统计（TLS 层明文之外的计数）
	var reader io.Reader = client
	if br != nil && br.Buffered() > 0 {
		reader = io.MultiReader(br, client)
	}
	counted := &countingConn{Conn: &bufConn{Conn: client, r: reader}, in: &p.c.bytesIn, out: &p.c.bytesOut}

	tlsConn := tls.Server(counted, p.tlsServerConfig())
	_ = client.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		p.log.Debug("MITM 客户端握手失败", "err", err)
		return
	}
	_ = client.SetDeadline(time.Time{})
	h := p.mitmHandlerSnapshot()
	if h == nil {
		p.log.Debug("无 SNI 反代未装配，放弃本地解密连接")
		return
	}
	p.serveOneConn(tlsConn, p.withDefaultPort(h, defaultPort))
}

// withDefaultPort 为 Host 头缺端口的请求补齐出站拨号端口（CONNECT 目标端口）。
// 只写入 URL.Host 供 Transport 拨号，不改写 Host 头本身。
func (p *Proxy) withDefaultPort(next http.Handler, port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if port != "" && r.URL.Host == "" {
			if _, _, err := net.SplitHostPort(r.Host); err != nil {
				r.URL.Host = net.JoinHostPort(r.Host, port)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// tlsServerConfig 返回 MITM 服务端 TLS 配置：按 SNI 动态选证书，只允许 HTTP/1.1。
// 证书与名单在握手开始时取一次快照，避免与运行时的启停相互竞态。
func (p *Proxy) tlsServerConfig() *tls.Config {
	ca, match := p.mitmComponents()
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := strings.ToLower(strings.TrimSuffix(chi.ServerName, "."))
			if name == "" {
				return nil, errors.New("缺少 SNI，无法选择证书")
			}
			if ca == nil || match == nil {
				return nil, errors.New("无 SNI 反代未启用")
			}
			if !match(name) {
				return nil, fmt.Errorf("域名 %s 不在反代名单", name)
			}
			return ca.Leaf(name)
		},
	}
}

// serveOneConn 以单连接 http.Server 方式运行 handler（支持 keep-alive 与101升级）。
func (p *Proxy) serveOneConn(conn net.Conn, handler http.Handler) {
	l := &singleConnListener{conn: conn, addr: conn.RemoteAddr(), done: make(chan struct{})}
	_ = (&http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}).Serve(l)
}

// dialTLSNoSNI 是 MITM 出站拨号：候选 IP 逐个 TCP+TLS 探测，握手不携带 SNI。
func (p *Proxy) dialTLSNoSNI(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "443"
	}

	// 单个候选：TCP + 无 SNI TLS 握手
	dialOne := func(cctx context.Context, ip net.IP) (net.Conn, error) {
		return p.dialOneNoSNI(cctx, network, port, ip)
	}

	// 快路径：已知优选 IP 直接拨号，完全跳过 DoH 查询（显著降低二次请求延迟）。
	if ip, ok := p.goodIP(host); ok {
		if c, err := dialOne(ctx, ip); err == nil {
			return c, nil
		}
		p.forgetGood(host)
	}

	candidates := p.mitmCandidates(ctx, host)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s 无干净候选 IP（DoH 全部失败）", host)
	}

	// 其余候选并发竞速，首个握手成功者胜出
	c, winIP, err := dialRace(ctx, candidates, dialOne)
	if err != nil {
		return nil, fmt.Errorf("无 SNI 拨号全部失败（%d 个候选）: %w", len(candidates), err)
	}
	p.rememberGood(host, winIP)
	p.log.Debug("无 SNI 拨号成功", "host", host, "ip", winIP)
	return c, nil
}

// dialOneNoSNI 建立到单个候选 IP 的 TCP 连接并完成「不携带 SNI」的 TLS 握手。
// 供真实请求与启动预热（warm.go）共用，保证两条路径的候选语义与优选缓存完全一致。
func (p *Proxy) dialOneNoSNI(cctx context.Context, network, port string, ip net.IP) (net.Conn, error) {
	raw, derr := p.dialer.DialContext(cctx, network, net.JoinHostPort(ip.String(), port))
	if derr != nil {
		return nil, derr
	}
	// 关键：ServerName 为空 → ClientHello 不携带 SNI 扩展
	tc := tls.Client(raw, &tls.Config{
		ServerName:         "",
		InsecureSkipVerify: true, // 无法在无 SNI 下校验证书；对浏览器的正确性由本地 MITM 证书保证
		NextProtos:         []string{"http/1.1"},
		MinVersion:         tls.VersionTLS12,
	})
	hctx, cancel := context.WithTimeout(cctx, 8*time.Second)
	defer cancel()
	if herr := tc.HandshakeContext(hctx); herr != nil {
		_ = raw.Close()
		return nil, herr
	}
	return tc, nil
}

// mitmCandidates 返回候选 IP：静态/远程优选在前，其后是干净 DoH 并查结果。
// 用 LookupAllUncached（而非带缓存的 LookupAll）——优选需要真实可达的候选，
// 而 DoH 缓存可能已过期或指向已被 GFW 污染的 IP。
func (p *Proxy) mitmCandidates(ctx context.Context, host string) []net.IP {
	var out []net.IP
	seen := make(map[string]bool)

	var ips []net.IP
	lookup := p.lookupAllUncached
	if lookup == nil {
		lookup = p.lookupAll
	}
	if lookup != nil {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		ips = lookup(cctx, host)
		cancel()
	}
	for _, ip := range ips {
		if ip == nil || ip.IsUnspecified() {
			continue
		}
		if !p.allowLoopback && (ip.IsLoopback() || ip.IsPrivate()) {
			continue
		}
		key := ip.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, normalizeCandidate(ip))
	}
	return out
}

// normalizeCandidate 把 4-in-6 形式的地址压成 IPv4，其余（真正的 IPv6）原样保留。
// 注意不能直接写 ip.To4()：对 IPv6 它返回 nil，会把候选变成空地址。
func normalizeCandidate(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

const goodTTL = 10 * time.Minute

func (p *Proxy) goodIP(host string) (net.IP, bool) {
	p.goodMu.Lock()
	defer p.goodMu.Unlock()
	e, ok := p.good[host]
	if !ok || !e.exp.After(time.Now()) {
		return nil, false
	}
	return e.ip, true
}

func (p *Proxy) rememberGood(host string, ip net.IP) {
	p.goodMu.Lock()
	defer p.goodMu.Unlock()
	p.good[host] = goodEntry{ip: ip, exp: time.Now().Add(goodTTL)}
}

func (p *Proxy) forgetGood(host string) {
	p.goodMu.Lock()
	defer p.goodMu.Unlock()
	delete(p.good, host)
}

// ListenLocal443 绑定 127.0.0.1:443：hosts 模式下让不走系统代理的程序
// （如 Steam 客户端）直连本地完成 MITM。绑定失败返回错误（调用方降级为真实 IP）。
//
// 同时尽力而为地再绑一份 [::1]:443：域名自己的 AAAA 记录会让 IPv6 优先的客户端
// 绕过本地捕捉（详见 hosts.Manager.SetLocalIPv6）。
func (p *Proxy) ListenLocal443() error {
	if !p.MITMEnabled() {
		return fmt.Errorf("MITM 未启用")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		return err
	}
	p.localMu.Lock()
	p.ln443 = ln
	p.localMu.Unlock()
	go p.acceptLoop(ln, true)
	p.listenLocalIPv6(443, true)
	return nil
}

// ListenLocal80 绑定 127.0.0.1:80：hosts 模式下的 HTTP 请求转发（无需解密）。
func (p *Proxy) ListenLocal80() error {
	ln, err := net.Listen("tcp", "127.0.0.1:80")
	if err != nil {
		return err
	}
	p.localMu.Lock()
	p.ln80 = ln
	p.localMu.Unlock()
	go p.acceptLoop(ln, false)
	p.listenLocalIPv6(80, false)
	return nil
}

// listenLocalIPv6 尽力而为地绑定 [::1]:port，返回是否成功。
// 失败只记 Debug：这只影响「IPv6 优先的客户端是否被本地捕捉」，不影响 IPv4 路径。
func (p *Proxy) listenLocalIPv6(port int, isTLS bool) bool {
	addr := net.JoinHostPort("::1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.log.Debug("本地 IPv6 回环监听不可用（hosts 只写 IPv4）", "addr", addr, "err", err)
		return false
	}
	p.localMu.Lock()
	if isTLS {
		p.ln443v6 = ln
	} else {
		p.ln80v6 = ln
	}
	p.localMu.Unlock()
	go p.acceptLoop(ln, isTLS)
	p.log.Debug("本地 IPv6 回环监听已就绪", "addr", addr)
	return true
}

// LocalIPv6Up 报告 [::1]:443 的本地监听是否就绪（决定 hosts 里是否写 ::1）。
func (p *Proxy) LocalIPv6Up() bool {
	p.localMu.Lock()
	defer p.localMu.Unlock()
	return p.ln443v6 != nil
}

// CloseLocal 关闭本地直连监听。
func (p *Proxy) CloseLocal() {
	p.localMu.Lock()
	ln443, ln80 := p.ln443, p.ln80
	ln443v6, ln80v6 := p.ln443v6, p.ln80v6
	p.ln443, p.ln80 = nil, nil
	p.ln443v6, p.ln80v6 = nil, nil
	p.localMu.Unlock()
	for _, ln := range []net.Listener{ln443, ln80, ln443v6, ln80v6} {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

func (p *Proxy) acceptLoop(ln net.Listener, isTLS bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // 监听已关闭
		}
		p.c.active.Add(1)
		go func() {
			defer p.c.active.Add(-1)
			if isTLS {
				p.serveMITMStream(conn, nil, "443")
				return
			}
			defer conn.Close()
			// HTTP 明文：无需解密，复用代理处理器转发（Host 头路由到干净 IP）
			p.serveOneConn(&countingConn{Conn: conn, in: &p.c.bytesIn, out: &p.c.bytesOut}, p)
		}()
	}
}

// singleConnListener 只接受一个连接的 net.Listener（单连接 http.Server 模式）。
// 投递连接后的第二次 Accept 会阻塞到连接真正结束，避免 Serve 提前返回。
type singleConnListener struct {
	conn net.Conn
	addr net.Addr
	once sync.Once
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		c = &closeSignalConn{Conn: l.conn, done: l.done}
	})
	if c == nil {
		<-l.done
		return nil, errors.New("singleConnListener: 已耗尽")
	}
	return c, nil
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return l.conn.LocalAddr()
}

// closeSignalConn 在连接被 http.Server 关闭时通知 listener。
type closeSignalConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *closeSignalConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// bufConn 在底层连接之上固定一个读取器（用于消费 Hijack 预读缓冲）。
type bufConn struct {
	net.Conn
	r io.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
