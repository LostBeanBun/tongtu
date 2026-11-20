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
	SetMode(sys bool) error
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

	// Layout 返回当前窗口的几何快照（/ui/layout），没有窗口时返回 nil。
	// 自绘控件没有 UI Automation 信息，真机冒烟脚本靠它按 ID 精确点击。
	// 只读、仅回环，不参与任何加速逻辑。
	Layout func() any
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
	mux.HandleFunc("/ui/layout", s.handleLayout)
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
	sys, ok := parseBool(q.Get("sysproxy"))
	if !ok {
		http.Error(w, "参数需要 sysproxy 布尔值", http.StatusBadRequest)
		return
	}
	if err := s.api.SetMode(sys); err != nil {
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

// handleLayout 返回自绘控件的几何快照（客户区坐标）。
// 窗口还没创建（例如 --no-tray 且界面创建失败）时返回 404，让调用方知道自己没界面可点。
func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Layout == nil {
		http.Error(w, "no window", http.StatusNotFound)
		return
	}
	snap := s.Layout()
	if snap == nil {
		http.Error(w, "window not painted yet", http.StatusNotFound)
		return
	}
	writeJSON(w, snap)
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

// PortState 是控制端口探测的结论（ProbeControlState）。
//
// 三者分流的口径：stop / status 只对**确认是通途**的端口发指令、报「已在运行」；
// start 对「确认是别的 HTTP 服务」的端口换口继续；识别不出的占用者单独成一类，
// 既不当通途也不当可换口的外来服务（保守定位见 PortUnknown 注释）。
type PortState int

const (
	// PortFree 端口无人监听。
	PortFree PortState = iota
	// PortTongtu 确认跑着一个通途实例：/status 返回我们认识的状态形状。
	PortTongtu
	// PortForeignHTTP 确认是一个「不是通途」的 HTTP 服务：响应了请求，但形状不对。
	PortForeignHTTP
	// PortUnknown 有人监听，但拿不到可识别的响应（非 HTTP 程序占用，或通途
	// 恰好没在超时内应答）。
	//
	// 保守定位（两个方向都不越界）：
	//   - 不当作通途：stop / status 不能对陌生进程发指令或报「已在运行」；
	//   - 不当作可确认的外来服务：start 只拒绝启动并说明真实原因，**不换口**
	//     （万一占用者只是响应超时的通途，换口就会起出第二个实例）。
	PortUnknown
)

// ProbeControlState 探测控制端口上的占用情况，供单实例检测与
// 「端口被别的程序占用」的分流使用。
func ProbeControlState(addr string) PortState {
	if !Running(addr) {
		return PortFree
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	body, err := Call(ctx, addr, "/status", http.MethodGet)
	if err != nil {
		return PortUnknown
	}
	var probe struct {
		Listen  string `json:"listen"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || (!probe.Running && probe.Listen == "") {
		return PortForeignHTTP // 是个 HTTP 服务，但形状不对：别的程序
	}
	return PortTongtu
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
