package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"tongtu/internal/mitm"
	"tongtu/internal/rules"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustRules(t *testing.T) *rules.Set {
	t.Helper()
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("rules.Load: %v", err)
	}
	return rs
}

// failLookup 记录不应发生的解析调用。
func failLookup(t *testing.T, note string) LookupFunc {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		t.Errorf("%s: 不应触发白名单解析（host=%s）", note, host)
		return nil, fmt.Errorf("不应解析")
	}
}

// localLookup 让白名单主机解析到本地回环。
func localLookup(t *testing.T, want string) (LookupFunc, *int) {
	var calls int
	return func(ctx context.Context, host string) ([]net.IP, error) {
		calls++
		if host != want {
			t.Errorf("解析主机错误: got %q want %q", host, want)
		}
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}, &calls
}

func TestPlainHTTPForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-http")
	}))
	defer upstream.Close()

	p := NewWithLookup(mustRules(t), failLookup(t, "HTTP 转发"), discardLog())
	px := httptest.NewServer(p)
	defer px.Close()

	pxURL, _ := url.Parse(px.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}
	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-http" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if snap := p.Snapshot(); snap.Requests != 1 {
		t.Fatalf("请求数统计错误: %d", snap.Requests)
	}
}

// startTLSUpstream 启动一个本地 TLS 服务，返回端口。
func startTLSUpstream(t *testing.T) (port string) {
	t.Helper()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-tls")
	}))
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// connectByHandshake 手工执行 CONNECT 隧道并完成一次 TLS GET 请求。
func connectByHandshake(t *testing.T, proxyAddr, target string) (string, error) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		return "", fmt.Errorf("CONNECT 被拒绝: %s%s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	host, _, _ := net.SplitHostPort(target)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		return "", fmt.Errorf("TLS 握手失败: %w", err)
	}
	if _, err := io.WriteString(tlsConn, "GET /probe HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func TestConnectWhitelistUsesInjectedLookup(t *testing.T) {
	port := startTLSUpstream(t)
	lookup, calls := localLookup(t, "steamcommunity.com")
	p := NewWithLookup(mustRules(t), lookup, discardLog())
	px := httptest.NewServer(p)
	defer px.Close()

	target := net.JoinHostPort("steamcommunity.com", port)
	body, err := connectByHandshake(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("隧道测试失败: %v", err)
	}
	if body != "hello-tls" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if *calls != 1 {
		t.Fatalf("期望调用解析 1 次，实际 %d 次", *calls)
	}
	if snap := p.Snapshot(); snap.Tunnels != 1 || snap.BytesIn == 0 || snap.BytesOut == 0 {
		t.Fatalf("隧道统计异常: %+v", snap)
	}
}

func TestConnectDirectSkipsLookup(t *testing.T) {
	port := startTLSUpstream(t)
	p := NewWithLookup(mustRules(t), failLookup(t, "非白名单 CONNECT"), discardLog())
	px := httptest.NewServer(p)
	defer px.Close()

	target := net.JoinHostPort("127.0.0.1", port)
	body, err := connectByHandshake(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("直通隧道失败: %v", err)
	}
	if body != "hello-tls" {
		t.Fatalf("响应内容错误: %q", body)
	}
}

// TestMITMStripsSNI 验证无 SNI 反代：客户端用真实 SNI 发起 CONNECT，
// 本地解密后由本代理转发，但出站 TLS 握手不携带 SNI。
func TestMITMStripsSNI(t *testing.T) {
	// 上游记录握手收到的 SNI（应为空）
	var gotSNI atomic.Value
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "steamcommunity.com" {
			http.Error(w, "wrong host: "+r.Host, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "hello-mitm")
	}))
	up.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		gotSNI.Store(chi.ServerName)
		return nil, nil
	}}
	up.StartTLS()
	defer up.Close()
	upPort := func() string { u, _ := url.Parse(up.URL); return u.Port() }()

	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	p := NewWithLookup(mustRules(t), failLookup(t, "MITM 路径不应触发白名单解析"), discardLog())
	p.allowLoopback = true
	p.SetLookupAll(func(ctx context.Context, host string) []net.IP {
		return []net.IP{net.IPv4(127, 0, 0, 1)}
	})
	p.EnableMITM(ca, func(h string) bool { return h == "steamcommunity.com" })
	px := httptest.NewServer(p)
	defer px.Close()

	// 客户端带真实 SNI=steamcommunity.com 走 CONNECT；代理应剥掉 SNI 后再出站
	target := net.JoinHostPort("steamcommunity.com", upPort)
	body, err := connectByHandshake(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("MITM 隧道失败: %v", err)
	}
	if body != "hello-mitm" {
		t.Fatalf("响应内容错误: %q", body)
	}
	if sni, _ := gotSNI.Load().(string); sni != "" {
		t.Fatalf("出站握手不应携带 SNI，实际为 %q", sni)
	}
	if snap := p.Snapshot(); snap.Tunnels != 1 {
		t.Fatalf("MITM 连接应计入隧道: %+v", snap)
	}
}

// connectPeerCert 完成一次 CONNECT + TLS 握手，返回客户端看到的叶证书 DER 与响应体。
func connectPeerCert(t *testing.T, proxyAddr, target string) ([]byte, string, error) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, "", fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		return nil, "", fmt.Errorf("CONNECT 被拒绝: %s%s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, "", err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	host, _, _ := net.SplitHostPort(target)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		return nil, "", fmt.Errorf("TLS 握手失败: %w", err)
	}
	peers := tlsConn.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		return nil, "", fmt.Errorf("未收到对端证书")
	}
	raw := peers[0].Raw

	if _, err := io.WriteString(tlsConn, "GET /probe HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
		return nil, "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, "", fmt.Errorf("读取响应失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return raw, string(body), nil
}

// TestMITMRuntimeToggle 验证「无 SNI 反代」可以随时热停用：
// 启用时客户端拿到的是本地 CA 签发的证书；停用后同样的 CONNECT 回退为普通隧道，
// 客户端直接看到上游证书。
func TestMITMRuntimeToggle(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-tls")
	}))
	up.StartTLS()
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	upRaw := up.Certificate().Raw

	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	p := NewWithLookup(mustRules(t), failLookup(t, "停用反代后按白名单隧道"), discardLog())
	p.allowLoopback = true
	p.SetLookupAll(func(context.Context, string) []net.IP { return []net.IP{net.IPv4(127, 0, 0, 1)} })
	p.EnableMITM(ca, func(h string) bool { return h == "steamcommunity.com" })
	if !p.MITMEnabled() {
		t.Fatal("EnableMITM 之后应报告已启用")
	}
	px := httptest.NewServer(p)
	defer px.Close()

	target := net.JoinHostPort("steamcommunity.com", u.Port())

	raw, body, err := connectPeerCert(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("启用反代时连接失败: %v", err)
	}
	if body != "hello-tls" {
		t.Fatalf("启用反代时响应错误: %q", body)
	}
	leaf, err := ca.Leaf("steamcommunity.com")
	if err != nil {
		t.Fatalf("签发本地证书失败: %v", err)
	}
	if !bytes.Equal(raw, leaf.Certificate[0]) {
		t.Fatal("启用反代时应返回本地 CA 签发的证书")
	}

	p.DisableMITM()
	if p.MITMEnabled() {
		t.Fatal("DisableMITM 之后应报告已停用")
	}

	raw, body, err = connectPeerCert(t, px.Listener.Addr().String(), target)
	if err != nil {
		t.Fatalf("停用反代后连接失败: %v", err)
	}
	if body != "hello-tls" {
		t.Fatalf("停用反代后响应错误: %q", body)
	}
	if !bytes.Equal(raw, upRaw) {
		t.Fatal("停用反代后应直传上游证书（说明已回退为普通隧道）")
	}
}
