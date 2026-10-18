package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tongtu/internal/mitm"
)

// TestPlainHTTPStripsAltSvc 普通 HTTP 转发也应去掉上游的 Alt-Svc 广告。
func TestPlainHTTPStripsAltSvc(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		fmt.Fprint(w, "ok")
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
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("Alt-Svc 应被剥离，实际为 %q", got)
	}
}

// TestMITMStripsAltSvc 反代（本地解密）路径同样剥离 Alt-Svc：
// hosts 模式下域名指向 127.0.0.1，留着它会让浏览器去试注定失败的 QUIC。
func TestMITMStripsAltSvc(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		fmt.Fprint(w, "hello-mitm")
	}))
	up.StartTLS()
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	p := NewWithLookup(mustRules(t), failLookup(t, "MITM 路径不应触发白名单解析"), discardLog())
	p.allowLoopback = true
	p.SetLookupAll(func(context.Context, string) []net.IP { return []net.IP{net.IPv4(127, 0, 0, 1)} })
	p.EnableMITM(ca, func(h string) bool { return h == "steamcommunity.com" })
	px := httptest.NewServer(p)
	defer px.Close()

	resp := connectMITMResponse(t, px.Listener.Addr().String(), net.JoinHostPort("steamcommunity.com", u.Port()))
	defer resp.Body.Close()
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("Alt-Svc 应被剥离，实际为 %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-mitm" {
		t.Fatalf("响应内容错误: %q", body)
	}
}

// connectMITMResponse 走 CONNECT + 本地解密路径，返回解密后的 HTTP 响应。
func connectMITMResponse(t *testing.T, proxyAddr, target string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取 CONNECT 响应失败: %v", err)
	}
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		t.Fatalf("CONNECT 被拒绝: %s%s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	host, _, _ := net.SplitHostPort(target)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS 握手失败: %v", err)
	}
	if _, err := io.WriteString(tlsConn, "GET /probe HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp
}
