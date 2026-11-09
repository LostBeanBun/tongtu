package proxy

import (
	"bufio"
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
	"testing"

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
