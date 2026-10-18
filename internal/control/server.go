// Package control 提供仅回环的本地控制 API：状态查询、模式切换与优雅停止。
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"tongtu/internal/app"
)

// API 是控制端点背后的能力接口（由 app.App 实现）。
type API interface {
	Status() app.Status
	SetMode(sys, hosts bool) error
}

// Server 是回环 HTTP 控制服务。
type Server struct {
	addr string
	api  API
	log  *slog.Logger
	ln   net.Listener
	srv  *http.Server

	// OnShutdown 在收到 /shutdown 后异步触发（由调用方注入退出逻辑）。
	OnShutdown func()
}

// New 创建控制服务（未监听）。
func New(addr string, api API, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{addr: addr, api: api, log: log}
}

// Start 绑定控制端口。绑定失败通常意味着已有实例在运行。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("控制端口 %s 不可用（可能已有实例在运行）: %w", s.addr, err)
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/mode", s.handleMode)
	mux.HandleFunc("/shutdown", s.handleShutdown)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("控制服务退出", "err", err)
		}
	}()
	return nil
}

// Shutdown 关闭控制服务。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.api.Status())
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	sys, ok1 := parseBool(q.Get("sysproxy"))
	hosts, ok2 := parseBool(q.Get("hosts"))
	if !ok1 || !ok2 {
		http.Error(w, "参数需要 sysproxy 与 hosts 两个布尔值", http.StatusBadRequest)
		return
	}
	if err := s.api.SetMode(sys, hosts); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": s.api.Status()})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	if s.OnShutdown != nil {
		go s.OnShutdown()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func parseBool(s string) (bool, bool) {
	switch s {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	}
	return false, false
}

// Running 探测 addr 上是否有进程在监听（用于代理端口的存活检查）。
//
// 注意：只看「端口有人监听」，不判断对方是不是通途——它被用来探测**代理端口**，
// 那里没有 HTTP 控制 API。要判断控制端口上是不是通途，请用 ProbeControl。
func Running(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeControl 探测控制端口上的占用情况，供单实例检测与「端口被别的程序占用」分流使用：
//
//	occupied：端口有人监听
//	foreign： 监听者明确是一个「不是通途」的 HTTP 服务
//
// 若端口有人监听但拿不到我们认识的 TCP/HTTP 响应（例如被非 HTTP 程序占用），
// foreign=false——按「可能就是通途自己」保守处理，避免误判成「可以再起一个实例」。
func ProbeControl(addr string) (occupied, foreign bool) {
	if !Running(addr) {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	body, err := Call(ctx, addr, "/status", http.MethodGet)
	if err != nil {
		return true, false // 有人监听，但不是我们能识别的 HTTP 服务：保守当作通途
	}
	var probe struct {
		Listen  string `json:"listen"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || (!probe.Running && probe.Listen == "") {
		return true, true // 是个 HTTP 服务，但形状不对：别的程序
	}
	return true, false
}

// IsTongtuControl 报告控制端口上是否确实跑着一个通途实例。
func IsTongtuControl(addr string) bool {
	occupied, foreign := ProbeControl(addr)
	return occupied && !foreign
}

// FreeLoopbackAddr 在 addr 所在的回环主机上找一个空闲端口，返回 "host:port"。
// 用于控制端口被别的程序占用时自动换口；调用方应把新地址写回配置，
// 否则 tongtu status / stop 之后会连不上这个实例。
func FreeLoopbackAddr(addr string) (string, bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", false
	}
	defer ln.Close()
	return ln.Addr().String(), true
}

// Call 发起一次控制请求并返回响应体。
func Call(ctx context.Context, addr, path, method string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
