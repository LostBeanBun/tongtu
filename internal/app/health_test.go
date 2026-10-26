package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tongtu/internal/config"
	"tongtu/internal/resolver"
	"tongtu/internal/rules"
)

// newHealthTestApp 创建一个只监听回环随机端口的 App，测试结束自动停止。
func newHealthTestApp(t *testing.T, cfg *config.Config) *App {
	t.Helper()
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	if cfg == nil {
		cfg = config.Default()
	}
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.Hosts, cfg.MITM.Enabled = false, false, false
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a
}

// TestHealthStoppedAndPaused 健康度状态机的基本状态词。
func TestHealthStoppedAndPaused(t *testing.T) {
	a := newHealthTestApp(t, nil)
	if h := a.CheckHealth(); h.State != HealthStopped {
		t.Fatalf("未启动时健康度应为 stopped，实际 %q", h.State)
	}

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := a.CheckHealth(); h.State != HealthOK {
		t.Fatalf("启动后健康度应为 ok，实际 %q（%s）", h.State, h.Reason)
	}

	a.mu.Lock()
	a.paused = &pauseState{mitm: true}
	a.mu.Unlock()
	if h := a.CheckHealth(); h.State != HealthPaused {
		t.Fatalf("暂停时健康度应为 paused，实际 %q", h.State)
	}
}

// TestCheckHealthRestartsDeadProxy 本地监听被关闭后，健康检查要能重新绑上，
// 而不是一直显示「运行中」却什么都连不上。
func TestCheckHealthRestartsDeadProxy(t *testing.T) {
	a := newHealthTestApp(t, nil)
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		t.Fatal("启动后应有代理服务")
	}
	before := a.Status().Health.HealCount
	if err := srv.Close(); err != nil {
		t.Fatalf("关闭代理监听: %v", err)
	}

	// 关掉监听后有两种情况都可以接受：端口已经真的不可连（接下来由本次
	// CheckHealth 修复），或后台健康检查先一步完成了自愈。后者在 CI 上确实
	// 会发生——healthLoop 的首次检查与这里的 Close 是并发的。
	// 等其中一种成立即可，避免把「后台先修好了」误判成前置条件失败。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !addrAlive(a.ProxyAddr()) || a.Status().Health.HealCount > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	h := a.CheckHealth()
	if h.HealCount <= before {
		t.Fatalf("健康检查应执行一次自愈: %+v", h)
	}
	if h.State != HealthOK {
		t.Fatalf("自愈后健康度应为 ok，实际 %q（%s）", h.State, h.Reason)
	}
	if !addrAlive(a.ProxyAddr()) {
		t.Fatal("自愈后本地代理应重新可连")
	}
	if !a.Status().Running {
		t.Fatal("自愈后服务仍应处于运行中")
	}
}

// TestRecoverNetworkClearsCachesAndDebounces 睡眠/换网恢复要清掉 DNS 缓存，
// 并把同一轮事件引发的重复触发合并成一次。
func TestRecoverNetworkClearsCachesAndDebounces(t *testing.T) {
	var hits atomic.Int64
	doh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Status": 0,
			"Answer": []map[string]any{{
				"name": r.URL.Query().Get("name"), "type": 1, "TTL": 120, "data": "1.2.3.4",
			}},
		})
	}))
	t.Cleanup(doh.Close)

	cfg := config.Default()
	cfg.DNS.Endpoints = []string{doh.URL}
	a := newHealthTestApp(t, cfg)
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.res.Lookup(ctx, "example.com"); err != nil {
		t.Fatalf("首次解析: %v", err)
	}
	if _, err := a.res.Lookup(ctx, "example.com"); err != nil {
		t.Fatalf("二次解析: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("缓存命中时应只有 1 次 DoH 请求，实际 %d", got)
	}

	if err := a.RecoverNetwork("测试换网"); err != nil {
		t.Fatalf("RecoverNetwork: %v", err)
	}
	if got := a.Status().Health.RecoverCount; got != 1 {
		t.Fatalf("应记录 1 次网络恢复，实际 %d", got)
	}
	if _, err := a.res.Lookup(ctx, "example.com"); err != nil {
		t.Fatalf("恢复后解析: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("恢复后应重新查询 DoH（缓存已清），实际 %d 次", got)
	}

	// 防抖：紧接着再触发一次不应真正执行。
	if err := a.RecoverNetwork("重复事件"); err != nil {
		t.Fatalf("重复 RecoverNetwork: %v", err)
	}
	if got := a.Status().Health.RecoverCount; got != 1 {
		t.Fatalf("防抖窗口内不应重复恢复，实际 %d 次", got)
	}

	// 清掉防抖时间戳后应能再次执行（模拟下一次睡眠/换网）。
	a.mu.Lock()
	a.lastRecover = time.Time{}
	a.mu.Unlock()
	if err := a.RecoverNetwork("下一轮换网"); err != nil {
		t.Fatalf("下一轮 RecoverNetwork: %v", err)
	}
	if got := a.Status().Health.RecoverCount; got != 2 {
		t.Fatalf("下一轮应再次恢复，实际 %d 次", got)
	}
}

// TestRecoverNetworkNoopWhenStopped 未启动 / 已停止时恢复是安全的空操作。
func TestRecoverNetworkNoopWhenStopped(t *testing.T) {
	a := newHealthTestApp(t, nil)
	if err := a.RecoverNetwork("未启动"); err != nil {
		t.Fatalf("未启动时不应报错: %v", err)
	}
	if got := a.Status().Health.RecoverCount; got != 0 {
		t.Fatalf("未启动时不应记录恢复次数，实际 %d", got)
	}
}

// TestDNSAllFailed 只有「所有端点都真的失败过」才算 DNS 全失败；
// 刚启动、还没用过的端点不应被误判。
func TestDNSAllFailed(t *testing.T) {
	if dnsAllFailed(nil) {
		t.Error("没有端点时不应算全失败")
	}
	if dnsAllFailed([]resolver.Health{{Endpoint: "https://dns.example"}}) {
		t.Error("从未使用过的端点不应算失败")
	}
	if !dnsAllFailed([]resolver.Health{
		{Endpoint: "a", LastError: "boom"},
		{Endpoint: "b", LastError: "boom"},
	}) {
		t.Error("所有端点都有错误时应算全失败")
	}
	if dnsAllFailed([]resolver.Health{
		{Endpoint: "a", LastError: "boom"},
		{Endpoint: "b"},
	}) {
		t.Error("只要还有一个端点没失败，就不算全失败")
	}
}

// TestFingerprintSnapshots 网络指纹必须与接口顺序无关，但地址变化要能看出来。
func TestFingerprintSnapshots(t *testing.T) {
	base := []ifaceSnapshot{
		{Name: "Wi-Fi", Flags: net.FlagUp, MTU: 1500, Addrs: []string{"192.168.1.2", "10.0.0.2"}},
		{Name: "Ethernet", Flags: net.FlagUp, MTU: 1500, Addrs: []string{"172.16.0.2"}},
	}
	reordered := []ifaceSnapshot{base[1], base[0]}
	if fingerprintSnapshots(base) != fingerprintSnapshots(reordered) {
		t.Error("接口顺序不应影响指纹")
	}
	if fingerprintSnapshots(base) == "" {
		t.Error("指纹不应为空")
	}

	changed := []ifaceSnapshot{
		{Name: "Wi-Fi", Flags: net.FlagUp, MTU: 1500, Addrs: []string{"192.168.1.3", "10.0.0.2"}},
		{Name: "Ethernet", Flags: net.FlagUp, MTU: 1500, Addrs: []string{"172.16.0.2"}},
	}
	if fingerprintSnapshots(base) == fingerprintSnapshots(changed) {
		t.Error("地址变化应改变指纹")
	}
	if fingerprintSnapshots(nil) == "" {
		t.Error("空快照也应有稳定指纹")
	}
}

// TestShouldRecover 指纹变化只在「当前有网且与基线不同」时触发恢复：
// 断网本身不恢复（否则会在没网时白跑一遍 hosts/DNS），网络回来时再恢复。
func TestShouldRecover(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		want       bool
	}{
		{"首次拿到网络", "", "abc", true},
		{"换网", "abc", "def", true},
		{"同一网络", "abc", "abc", false},
		{"断网", "abc", "", false},
		{"一直没网", "", "", false},
	}
	for _, tc := range cases {
		if got := shouldRecover(tc.prev, tc.next); got != tc.want {
			t.Errorf("%s: shouldRecover(%q, %q) = %v，期望 %v", tc.name, tc.prev, tc.next, got, tc.want)
		}
	}
}

// TestNetworkFingerprintStable 同一时刻连续读取应得到相同指纹。
func TestNetworkFingerprintStable(t *testing.T) {
	first := networkFingerprint()
	if first == "" {
		t.Skip("当前环境拿不到网络接口信息")
	}
	if second := networkFingerprint(); second != first {
		t.Fatalf("连续两次指纹不一致: %q vs %q", first, second)
	}
}
