package control

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestProbeControlNoListener 没有监听者时 occupied=false。
func TestProbeControlNoListener(t *testing.T) {
	addr := freeAddr(t)
	occupied, foreign := ProbeControl(addr)
	if occupied || foreign {
		t.Fatalf("空端口应报告未占用: occupied=%v foreign=%v", occupied, foreign)
	}
	if IsTongtuControl(addr) {
		t.Fatal("空端口不应被当作通途")
	}
}

// TestProbeControlForeignHTTP 别的 HTTP 服务占了端口时不能被当成通途——
// 否则 start 会误报「tongtu 已在运行」。
func TestProbeControlForeignHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hello":"world"}`)
	}))
	defer srv.Close()
	addr := hostPort(t, srv.URL)

	occupied, foreign := ProbeControl(addr)
	if !occupied || !foreign {
		t.Fatalf("别的 HTTP 服务应报告 occupied=true foreign=true: %v %v", occupied, foreign)
	}
	if IsTongtuControl(addr) {
		t.Fatal("别的 HTTP 服务不应被当作通途")
	}
}

// TestProbeControlTongtu 通途自己的 /status 形状应被识别。
func TestProbeControlTongtu(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"running":true,"listen":"127.0.0.1:19180","system_proxy":true}`)
	}))
	defer srv.Close()
	addr := hostPort(t, srv.URL)

	occupied, foreign := ProbeControl(addr)
	if !occupied || foreign {
		t.Fatalf("通途控制端口应报告 occupied=true foreign=false: %v %v", occupied, foreign)
	}
	if !IsTongtuControl(addr) {
		t.Fatal("通途控制端口应被识别为通途")
	}
}

// TestProbeControlNonHTTP 非 HTTP 程序占用端口时保守地不判定为「别的程序」。
func TestProbeControlNonHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // 直接断开，不给 HTTP 响应
		}
	}()

	occupied, foreign := ProbeControl(ln.Addr().String())
	if !occupied || foreign {
		t.Fatalf("非 HTTP 占用应保守报告 occupied=true foreign=false: %v %v", occupied, foreign)
	}
}

// TestFreeLoopbackAddr 返回可用的回环地址，且保留配置里的主机部分。
func TestFreeLoopbackAddr(t *testing.T) {
	addr, ok := FreeLoopbackAddr("127.0.0.1:19181")
	if !ok {
		t.Fatal("应能找到空闲端口")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("主机应保持 127.0.0.1: %q (%v)", addr, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("返回的地址应可立即监听: %v", err)
	}
	_ = ln.Close()
}

// TestFreeLoopbackAddrBadInput 非法输入退化为 127.0.0.1。
func TestFreeLoopbackAddrBadInput(t *testing.T) {
	addr, ok := FreeLoopbackAddr("19181")
	if !ok {
		t.Fatal("应能找到空闲端口")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("非法输入应退化为 127.0.0.1: %q (%v)", addr, err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
