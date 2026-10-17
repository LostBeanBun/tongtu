//go:build windows

package app

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tongtu/internal/sysproxy"
)

// TestRecoverStaleSkipsLiveOwner 覆盖「遗留清理前先看主人是否还在」：
// 状态文件里记的代理地址还能连上，说明另一个实例仍在接管（代理端口是它开的），
// RecoverStale 必须收手——否则会把对方正在用的系统代理静默清掉，而对方界面照旧显示「已启用」。
//
// 测试把 APPDATA 指到临时目录，因此不会碰到真实的 sysproxy.json。
func TestRecoverStaleSkipsLiveOwner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()
	writeState(t, ln.Addr().String())

	var buf bytes.Buffer
	RecoverStale(slog.New(slog.NewTextHandler(&buf, nil)))
	if !strings.Contains(buf.String(), "仍由另一个实例持有") {
		t.Errorf("仍有人接管时应当跳过清理，实际日志: %s", buf.String())
	}
}

// TestStaleOwnerAlive 覆盖判定本身：端口没人听（崩溃遗留）算遗留状态、该清；
// 有人听就是在用的接管、不该动。
func TestStaleOwnerAlive(t *testing.T) {
	// 先占一个端口再关掉，拿到一个「肯定没人监听」的地址
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	closedAddr := dead.Addr().String()
	_ = dead.Close()

	writeState(t, closedAddr)
	if got, live := staleOwnerAlive(); live {
		t.Errorf("没人监听的端口不该算作仍被持有: %s", got)
	}

	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer live.Close()
	writeState(t, live.Addr().String())
	if got, ok := staleOwnerAlive(); !ok || got != live.Addr().String() {
		t.Errorf("有人监听时应判定为仍在持有: got=%q ok=%v", got, ok)
	}
}

// writeState 把状态文件写进临时 APPDATA（StatePath 跟着 APPDATA 走）。
func writeState(t *testing.T, addr string) {
	t.Helper()
	t.Setenv("APPDATA", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(sysproxy.StatePath()), 0o755); err != nil {
		t.Fatalf("建状态目录失败: %v", err)
	}
	data := []byte(`{"addr":"` + addr + `","saved":{},"saved_at":"2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(sysproxy.StatePath(), data, 0o644); err != nil {
		t.Fatalf("写状态文件失败: %v", err)
	}
}
