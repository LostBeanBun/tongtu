package control

import (
	"encoding/json"
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

// TestHandleLayout 覆盖 /ui/layout：自绘控件没有无障碍信息，真机脚本靠它按 ID 点按钮。
// 没窗口 / 还没画过时必须明确返回 404，脚本据此报「没有界面可点」而不是乱点。
func TestHandleLayout(t *testing.T) {
	s := &Server{}

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleLayout(rec, httptest.NewRequest(http.MethodGet, "/ui/layout", nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("没注入 Layout 时应返回 404，实际 %d", rec.Code)
	}

	s.Layout = func() any { return nil }
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("窗口还没画过时返回 404，实际 %d", rec.Code)
	}

	s.Layout = func() any {
		return map[string]any{
			"dpi": 96, "client_w": 855, "client_h": 481, "paused": false,
			"regions": []map[string]any{
				{"id": "pause", "label": "暂停", "left": 508, "top": 37, "right": 562, "bottom": 61},
			},
		}
	}
	rec := get()
	if rec.Code != http.StatusOK {
		t.Fatalf("有几何时应返回 200，实际 %d", rec.Code)
	}
	var got struct {
		Regions []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Left  int32  `json:"left"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Regions) != 1 || got.Regions[0].ID != "pause" || got.Regions[0].Label != "暂停" || got.Regions[0].Left != 508 {
		t.Fatalf("几何内容不符: %+v", got.Regions)
	}

	rec = httptest.NewRecorder()
	s.handleLayout(rec, httptest.NewRequest(http.MethodPost, "/ui/layout", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("非 GET 应返回 405，实际 %d", rec.Code)
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
