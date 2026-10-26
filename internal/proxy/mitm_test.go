package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tongtu/internal/mitm"
)

// newMITMTestProxy 装配一个只信任指定域名、解析固定指向回环的代理。
// 回环候选仅用于测试：生产会过滤私网/回环地址。
func newMITMTestProxy(t *testing.T, hosts ...string) (*Proxy, *mitm.CA) {
	t.Helper()
	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("加载本地 CA: %v", err)
	}
	p := NewWithLookup(mustRules(t), failLookup(t, "MITM 测试不应触发白名单解析"), discardLog())
	p.allowLoopback = true
	p.SetLookupAll(func(context.Context, string) []net.IP {
		return []net.IP{net.IPv4(127, 0, 0, 1)}
	})
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[h] = true
	}
	p.EnableMITM(ca, func(h string) bool { return allowed[h] })
	return p, ca
}

// connectMITMConn 完成 CONNECT 与客户端侧 TLS 握手，返回解密后的连接。
func connectMITMConn(t *testing.T, proxyAddr, target string, cfg *tls.Config) *tls.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("连接代理: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("写 CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读 CONNECT 响应: %v", err)
	}
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		t.Fatalf("CONNECT 被拒绝: %s%s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 CONNECT 响应头: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	tc := tls.Client(conn, cfg)
	if err := tc.Handshake(); err != nil {
		t.Fatalf("客户端 TLS 握手: %v", err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

// mitmHandshakeErr 完成 CONNECT 后尝试 TLS 握手，返回握手错误（不因握手失败而终止测试）。
func mitmHandshakeErr(t *testing.T, proxyAddr, target string, cfg *tls.Config) error {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("连接代理: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("写 CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读 CONNECT 响应: %v", err)
	}
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		t.Fatalf("CONNECT 被拒绝: %s%s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 CONNECT 响应头: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return tls.Client(conn, cfg).Handshake()
}

// readMITMResponse 在已解密的连接上发送一条原始 HTTP 请求并读取响应。
func readMITMResponse(t *testing.T, tc *tls.Conn, request string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(tc, request); err != nil {
		t.Fatalf("写 HTTP 请求: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("读 HTTP 响应: %v", err)
	}
	return resp
}

// TestMITMHandlerRejectsUnlistedHost 本地解密处理器必须自带名单校验：
// 即使 CONNECT 被放行，解密后的 Host 不在名单内也要拒绝，避免被当成通用解密代理。
func TestMITMHandlerRejectsUnlistedHost(t *testing.T) {
	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	h := p.mitmHandlerSnapshot()
	if h == nil {
		t.Fatal("MITM 处理器未装配")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://evil.example/probe", nil)
	req.Host = "evil.example"
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("未授权 Host 应返回 421，实际 %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "host not allowed") {
		t.Fatalf("响应体应说明拒绝原因，实际 %q", rr.Body.String())
	}
}

// TestMITMTLSConfigSelectsLeafBySNI 验证服务端证书按 SNI 动态签发，并只允许 HTTP/1.1。
func TestMITMTLSConfigSelectsLeafBySNI(t *testing.T) {
	p, ca := newMITMTestProxy(t, "steamcommunity.com")
	cfg := p.tlsServerConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("最低 TLS 版本应为 1.2，实际 %#x", cfg.MinVersion)
	}
	if !slices.Equal(cfg.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("只应协商 HTTP/1.1，实际 %v", cfg.NextProtos)
	}

	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "SteamCommunity.COM."})
	if err != nil {
		t.Fatalf("名单内 SNI 应签发证书: %v", err)
	}
	want, err := ca.Leaf("steamcommunity.com")
	if err != nil {
		t.Fatalf("签发参考证书: %v", err)
	}
	if len(got.Certificate) == 0 || !bytes.Equal(got.Certificate[0], want.Certificate[0]) {
		t.Fatal("应按归一化后的 SNI 命中同一张叶证书")
	}

	for _, name := range []string{"evil.example", ""} {
		if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Errorf("SNI %q 应被拒绝", name)
		}
	}
}

// TestMITMRejectsMissingOrUnknownSNI 客户端不提供 SNI、或提供名单外 SNI 时，握手必须失败。
func TestMITMRejectsMissingOrUnknownSNI(t *testing.T) {
	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	px := httptest.NewServer(p)
	defer px.Close()

	target := "steamcommunity.com:443"
	for _, tc := range []struct {
		name string
		cfg  *tls.Config
	}{
		{"缺少 SNI", &tls.Config{InsecureSkipVerify: true}},
		{"未知 SNI", &tls.Config{ServerName: "evil.example", InsecureSkipVerify: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := mitmHandshakeErr(t, px.Listener.Addr().String(), target, tc.cfg); err == nil {
				t.Fatal("握手应被拒绝")
			}
		})
	}
}

// TestMITMOnlyAllowlistedHostsAreDecrypted 名单外域名即使启用了 MITM 也必须保持普通隧道，
// 客户端应看到上游真实证书，而不是本地 CA 签发的证书。
func TestMITMOnlyAllowlistedHostsAreDecrypted(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-tls")
	}))
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	px := httptest.NewServer(p)
	defer px.Close()

	target := net.JoinHostPort("127.0.0.1", u.Port())
	raw, body, err := connectPeerCert(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("名单外 CONNECT 失败: %v", err)
	}
	if body != "hello-tls" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if !bytes.Equal(raw, up.Certificate().Raw) {
		t.Fatal("名单外域名应直传上游证书（保持端到端隧道）")
	}
}

// TestMITMRoutesByHostHeaderNotSNI 出站 SNI 已剥离，站点路由必须完全依赖 HTTP Host 头。
func TestMITMRoutesByHostHeaderNotSNI(t *testing.T) {
	var gotHost, gotURI, gotSNI atomic.Value
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		gotURI.Store(r.URL.RequestURI())
		fmt.Fprint(w, "routed")
	}))
	up.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		gotSNI.Store(chi.ServerName)
		return nil, nil
	}}
	up.StartTLS()
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, _ := newMITMTestProxy(t, "steamcommunity.com", "raw.githubusercontent.com")
	px := httptest.NewServer(p)
	defer px.Close()

	tc := connectMITMConn(t, px.Listener.Addr().String(),
		net.JoinHostPort("steamcommunity.com", u.Port()),
		&tls.Config{ServerName: "steamcommunity.com", InsecureSkipVerify: true})
	resp := readMITMResponse(t, tc, "GET /route?x=1 HTTP/1.1\r\nHost: raw.githubusercontent.com\r\nConnection: close\r\n\r\n")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "routed" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if gotHost.Load() != "raw.githubusercontent.com" {
		t.Fatalf("上游应按 Host 头路由，实际 Host=%v", gotHost.Load())
	}
	if gotURI.Load() != "/route?x=1" {
		t.Fatalf("请求路径/查询串应保持原样，实际 %v", gotURI.Load())
	}
	if gotSNI.Load() != "" {
		t.Fatalf("出站握手不应携带 SNI，实际 %v", gotSNI.Load())
	}
}

// TestMITMForwardsRequestTransparently 反代应保持方法、路径、查询串与请求体，
// 同时丢弃客户端伪造的代理链头。
func TestMITMForwardsRequestTransparently(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "host=%s uri=%s xff=%q fwd=%q pc=%q body=%q",
			r.Host, r.URL.RequestURI(), r.Header.Get("X-Forwarded-For"),
			r.Header.Get("Forwarded"), r.Header.Get("Proxy-Connection"), body)
	}))
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	px := httptest.NewServer(p)
	defer px.Close()

	tc := connectMITMConn(t, px.Listener.Addr().String(),
		net.JoinHostPort("steamcommunity.com", u.Port()),
		&tls.Config{ServerName: "steamcommunity.com", InsecureSkipVerify: true})
	resp := readMITMResponse(t, tc,
		"POST /probe?a=1&b=2 HTTP/1.1\r\n"+
			"Host: steamcommunity.com\r\n"+
			"Content-Type: text/plain\r\n"+
			"Content-Length: 4\r\n"+
			"X-Forwarded-For: 203.0.113.9\r\n"+
			"Forwarded: for=203.0.113.9\r\n"+
			"Proxy-Connection: keep-alive\r\n"+
			"Connection: close\r\n\r\nping")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	want := `host=steamcommunity.com uri=/probe?a=1&b=2 xff="" fwd="" pc="" body="ping"`
	if string(body) != want {
		t.Fatalf("转发结果不符:\n got %s\nwant %s", body, want)
	}
}

// TestMITMUpstreamFailureReturnsBadGateway 上游不可达时应返回 502，而不是挂住连接。
func TestMITMUpstreamFailureReturnsBadGateway(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭临时监听: %v", err)
	}

	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	px := httptest.NewServer(p)
	defer px.Close()

	tc := connectMITMConn(t, px.Listener.Addr().String(),
		net.JoinHostPort("steamcommunity.com", fmt.Sprint(port)),
		&tls.Config{ServerName: "steamcommunity.com", InsecureSkipVerify: true})
	resp := readMITMResponse(t, tc, "GET /probe HTTP/1.1\r\nHost: steamcommunity.com\r\nConnection: close\r\n\r\n")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游不可达应返回 502，实际 %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "bad gateway") {
		t.Fatalf("502 响应体应包含错误说明，实际 %q", body)
	}
}

// TestMITMIPv6LoopbackCandidate 候选为 IPv6 时也必须能拨号（normalizeCandidate 不能把它变成 nil）。
func TestMITMIPv6LoopbackCandidate(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("本机没有 IPv6 回环: %v", err)
	}
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-ipv6")
	}))
	up.Listener = ln
	up.StartTLS()
	defer up.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("加载本地 CA: %v", err)
	}
	p := NewWithLookup(mustRules(t), failLookup(t, "IPv6 反代测试不应触发白名单解析"), discardLog())
	p.allowLoopback = true
	p.SetLookupAll(func(context.Context, string) []net.IP {
		return []net.IP{net.ParseIP("::1")}
	})
	p.EnableMITM(ca, func(h string) bool { return h == "steamcommunity.com" })
	px := httptest.NewServer(p)
	defer px.Close()

	body, err := connectByHandshake(t, px.Listener.Addr().String(),
		net.JoinHostPort("steamcommunity.com", fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("IPv6 反代失败: %v", err)
	}
	if body != "hello-ipv6" {
		t.Fatalf("响应内容错误: %q", body)
	}
	ip, ok := p.goodIP("steamcommunity.com")
	if !ok || !ip.Equal(net.ParseIP("::1")) {
		t.Fatalf("优选缓存应记录 IPv6 候选，实际 %v（命中=%v）", ip, ok)
	}
}

// TestMITMCandidatesFiltersAndDeduplicates 候选必须过滤空/未指定/回环/私网地址，并在归一化后去重。
func TestMITMCandidatesFiltersAndDeduplicates(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "候选测试不应触发白名单解析"), discardLog())
	p.SetLookupAll(func(context.Context, string) []net.IP {
		return []net.IP{
			net.IPv4(127, 0, 0, 1),
			net.IPv4(10, 0, 0, 1),
			net.ParseIP("::ffff:140.82.113.3"),
			net.ParseIP("140.82.113.3"),
			nil,
			net.IPv4zero,
			net.ParseIP("2606:50c0:8002::154"),
		}
	})

	got := ipStrings(p.mitmCandidates(context.Background(), "example.com"))
	want := []string{"140.82.113.3", "2606:50c0:8002::154"}
	if !slices.Equal(got, want) {
		t.Fatalf("默认候选过滤错误: got %v want %v", got, want)
	}

	p.allowLoopback = true
	got = ipStrings(p.mitmCandidates(context.Background(), "example.com"))
	want = []string{"127.0.0.1", "10.0.0.1", "140.82.113.3", "2606:50c0:8002::154"}
	if !slices.Equal(got, want) {
		t.Fatalf("允许回环时候选错误: got %v want %v", got, want)
	}
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// TestMITMCandidatesPrefersUncachedLookup 优选探测必须用无缓存解析，避免拿到过期的污染结果。
func TestMITMCandidatesPrefersUncachedLookup(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "候选测试不应触发白名单解析"), discardLog())
	p.SetLookupAll(func(context.Context, string) []net.IP {
		return []net.IP{net.IPv4(192, 0, 2, 1)}
	})
	p.SetLookupAllUncached(func(context.Context, string) []net.IP {
		return []net.IP{net.IPv4(198, 51, 100, 1)}
	})

	got := ipStrings(p.mitmCandidates(context.Background(), "example.com"))
	want := []string{"198.51.100.1"}
	if !slices.Equal(got, want) {
		t.Fatalf("应优先使用无缓存解析: got %v want %v", got, want)
	}
}

// TestMITMCandidatesEmptyWithoutLookup 没有注入解析器时不应 panic，也不应产生候选。
func TestMITMCandidatesEmptyWithoutLookup(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "无解析器时不应调用白名单解析"), discardLog())
	if got := p.mitmCandidates(context.Background(), "example.com"); len(got) != 0 {
		t.Fatalf("无解析器时应返回空候选，实际 %v", got)
	}
}

// TestGoodIPCacheLifecycle 优选缓存应支持写入、过期与主动遗忘。
func TestGoodIPCacheLifecycle(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "缓存测试不应触发白名单解析"), discardLog())
	ip := net.IPv4(203, 0, 113, 7)

	if _, ok := p.goodIP("example.com"); ok {
		t.Fatal("初始不应命中优选缓存")
	}
	p.rememberGood("example.com", ip)
	got, ok := p.goodIP("example.com")
	if !ok || !got.Equal(ip) {
		t.Fatalf("优选缓存命中错误: got %v ok=%v", got, ok)
	}

	p.goodMu.Lock()
	p.good["expired.example"] = goodEntry{ip: ip, exp: time.Now().Add(-time.Second)}
	p.goodMu.Unlock()
	if _, ok := p.goodIP("expired.example"); ok {
		t.Fatal("过期条目不应命中")
	}

	p.forgetGood("example.com")
	if _, ok := p.goodIP("example.com"); ok {
		t.Fatal("forgetGood 后不应再命中")
	}
}

// TestEnableMITMClearsGoodCache 重新装配反代时必须清空优选缓存，避免沿用旧名单/旧网络的结果。
func TestEnableMITMClearsGoodCache(t *testing.T) {
	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	p.rememberGood("steamcommunity.com", net.IPv4(203, 0, 113, 8))

	ca, match := p.mitmComponents()
	p.EnableMITM(ca, match)

	if _, ok := p.goodIP("steamcommunity.com"); ok {
		t.Fatal("重新启用反代后优选缓存应为空")
	}
}

// TestResetNetworkClearsGoodCache 换网/睡眠恢复后旧优选 IP 不再可信，必须整体作废。
func TestResetNetworkClearsGoodCache(t *testing.T) {
	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	p.rememberGood("steamcommunity.com", net.IPv4(203, 0, 113, 9))
	p.rememberGood("raw.githubusercontent.com", net.IPv4(203, 0, 113, 10))

	p.ResetNetwork()

	for _, host := range []string{"steamcommunity.com", "raw.githubusercontent.com"} {
		if _, ok := p.goodIP(host); ok {
			t.Fatalf("ResetNetwork 后 %s 不应再命中优选缓存", host)
		}
	}
	p.ResetNetwork() // 幂等
	if p.Local443Up() {
		t.Fatal("测试没有启动本地 :443，不应报告监听就绪")
	}
}

// TestMITMDisablesHTTP2 客户端与上游都不得协商 HTTP/2：反代按 HTTP/1.1 逐请求转发。
func TestMITMDisablesHTTP2(t *testing.T) {
	var upstreamProto atomic.Value
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamProto.Store(r.Proto)
		fmt.Fprint(w, "ok")
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, _ := newMITMTestProxy(t, "steamcommunity.com")
	px := httptest.NewServer(p)
	defer px.Close()

	tc := connectMITMConn(t, px.Listener.Addr().String(),
		net.JoinHostPort("steamcommunity.com", u.Port()),
		&tls.Config{
			ServerName:         "steamcommunity.com",
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2", "http/1.1"},
		})
	if got := tc.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("客户端应协商 HTTP/1.1，实际 %q", got)
	}

	resp := readMITMResponse(t, tc, "GET /probe HTTP/1.1\r\nHost: steamcommunity.com\r\nConnection: close\r\n\r\n")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if got := upstreamProto.Load(); got != "HTTP/1.1" {
		t.Fatalf("上游应收到 HTTP/1.1 请求，实际 %v", got)
	}
}
