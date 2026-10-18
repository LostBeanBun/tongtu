package hosts

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tongtu/internal/resolver"
)

// fakeDoH 返回固定 A 记录。
func fakeDoH(t *testing.T, data string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Status": 0,
			"Answer": []map[string]any{{"name": name, "type": 1, "TTL": 300, "data": data}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestManager(t *testing.T, path string, names []string) *Manager {
	t.Helper()
	doh := fakeDoH(t, "93.184.216.34")
	res := resolver.New(resolver.Config{
		Endpoints: []string{doh.URL},
		Timeout:   2 * time.Second,
		CacheMin:  time.Second,
		CacheMax:  time.Minute,
	}, nil)
	return New(path, names, res, time.Hour, nil)
}

func TestApplyAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	orig := "# original hosts\n127.0.0.1 localhost\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, path, []string{"steamcommunity.com", "github.com"})

	changed, err := m.Apply(context.Background())
	if err != nil || !changed {
		t.Fatalf("Apply: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if !strings.Contains(text, beginMark) || !strings.Contains(text, endMark) {
		t.Fatal("缺少托管块标记")
	}
	if !strings.Contains(text, "93.184.216.34\tsteamcommunity.com") {
		t.Fatal("缺少解析条目")
	}
	if !strings.Contains(text, "127.0.0.1 localhost") {
		t.Fatal("原有内容被破坏")
	}

	// 无变化时应报告 changed=false（不重写文件）
	changed, err = m.Apply(context.Background())
	if err != nil || changed {
		t.Fatalf("二次 Apply: changed=%v err=%v（期望无变化）", changed, err)
	}

	// 移除后应完全还原
	changed, err = m.Remove()
	if err != nil || !changed {
		t.Fatalf("Remove: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != orig {
		t.Fatalf("移除后未还原:\n%q\nwant\n%q", string(data), orig)
	}

	// 再次移除是幂等的
	changed, err = m.Remove()
	if err != nil || changed {
		t.Fatalf("重复 Remove: changed=%v err=%v", changed, err)
	}
}

func TestApplyPreservesCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	orig := "# windows hosts\r\n127.0.0.1 localhost\r\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, path, []string{"steamcommunity.com"})
	if _, err := m.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(strings.ReplaceAll(string(data), "\r\n", ""), "\n") {
		t.Fatal("混入了孤立的 LF 行尾")
	}
}

func TestUpsertBlockOnEmptyFile(t *testing.T) {
	block := beginMark + "\n1.2.3.4 a.com\n" + endMark + "\n"
	out, changed := upsertBlock("", block)
	if !changed {
		t.Fatal("空文件追加应报告变化")
	}
	if !strings.Contains(out, beginMark) {
		t.Fatal("追加失败")
	}
	out2, changed2 := upsertBlock(out, "")
	if !changed2 || strings.Contains(out2, beginMark) {
		t.Fatalf("删除失败: changed=%v out=%q", changed2, out2)
	}
}

func TestRenderBlockSkipsUnresolved(t *testing.T) {
	block := renderBlock([]string{"a.com", "b.com"}, map[string][]net.IP{
		"a.com": {net.ParseIP("1.1.1.1")},
	}, false)
	if strings.Contains(block, "b.com") {
		t.Fatal("未解析成功的主机不应写入")
	}
	if !strings.Contains(block, "1.1.1.1\ta.com") {
		t.Fatal("条目格式错误")
	}
}

// TestWriteHostsAtomic 回归保护：hosts 必须「先写临时文件再改名」写入。
// 直接原地覆盖时，进程在写入过程中被强杀 / 断电会留下被截断的 hosts，
// 那属于「异常退出破坏网络配置」，正是要避免的情况。
func TestWriteHostsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("原始内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHosts(path, []byte("新内容\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "新内容\n" {
		t.Errorf("hosts 内容不符: %q", got)
	}
	// 不应残留临时文件（否则会在 etc 目录里越积越多）
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留了临时文件: %s", e.Name())
		}
	}
}

// TestWriteHostsFallsBack 改名被占用挡住时（例如安全软件锁住 hosts）要退回直接写入，
// 宁可少一层保护也不能让 hosts 模式彻底失效。
func TestWriteHostsFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	// 用一个目录占住改名目标名下的临时文件名，让 os.Rename 一定失败
	if err := os.WriteFile(path, []byte("原始内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tongtu.tmp", 0o755); err != nil {
		t.Skipf("无法制造冲突（%v），跳过", err)
	}
	if err := writeHosts(path, []byte("兜底内容\n")); err != nil {
		t.Fatalf("兜底写入失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "兜底内容\n" {
		t.Errorf("兜底写入内容不符: %q", got)
	}
}

// countHandler 记录日志条数，用于观察 Run 的重试节奏。
type countHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

func (h *countHandler) count(sub string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// TestRunRetriesQuicklyAfterFailure 刷新失败时要用短周期重试，而不是等满一个刷新周期——
// 否则一次 DoH 抖动就会让托管块陈旧几十分钟（用户表现为「hosts 模式忽然不好使了」）。
func TestRunRetriesQuicklyAfterFailure(t *testing.T) {
	// 用一个目录当 hosts 路径：Apply 必然失败，且每次都是新失败
	path := filepath.Join(t.TempDir(), "as-dir")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// 端点指向一个没人监听的端口：解析也一定失败，不让测试触网
	res := resolver.New(resolver.Config{
		Endpoints: []string{"http://127.0.0.1:1/dns-query"},
		Timeout:   100 * time.Millisecond,
		CacheMin:  time.Second,
		CacheMax:  time.Minute,
	}, nil)
	h := &countHandler{}
	m := New(path, []string{"a.com"}, res, time.Hour, slog.New(h))
	m.retryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.count("稍后重试") < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未随 ctx 取消而退出")
	}
	if got := h.count("稍后重试"); got < 3 {
		t.Fatalf("失败后应快速重试（2 秒内至少 3 次），实际 %d 次", got)
	}
}
