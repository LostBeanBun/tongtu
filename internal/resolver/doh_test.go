package resolver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDoH 启动一个返回固定 A 记录的 DoH 模拟服务。
func fakeDoH(t *testing.T, data string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Status": 0,
			"Answer": []map[string]any{{"name": name, "type": 1, "TTL": 120, "data": data}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestResolver(endpoints ...string) *Resolver {
	return New(Config{
		Endpoints: endpoints,
		Timeout:   2 * time.Second,
		CacheMin:  time.Second,
		CacheMax:  time.Minute,
	}, nil)
}

func TestLookupSuccessAndCache(t *testing.T) {
	good, hits := fakeDoH(t, "1.2.3.4")
	r := newTestResolver(good.URL)

	ips, err := r.Lookup(context.Background(), "steamcommunity.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "1.2.3.4" {
		t.Fatalf("意外结果: %v", ips)
	}
	// 命中缓存，不应再次请求
	if _, err := r.Lookup(context.Background(), "steamcommunity.com"); err != nil {
		t.Fatalf("二次 Lookup: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("期望 1 次上游请求，实际 %d 次（缓存未生效）", n)
	}
}

func TestFailoverToSecondEndpoint(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	good, _ := fakeDoH(t, "5.6.7.8")
	r := newTestResolver(bad.URL, good.URL)

	ips, err := r.Lookup(context.Background(), "github.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "5.6.7.8" {
		t.Fatalf("意外结果: %v", ips)
	}
	// 故障端点应被移到队尾
	order := r.snapshotOrder()
	if order[0] != good.URL {
		t.Fatalf("失败端点未降权: %v", order)
	}
}

func TestLookupPublicFiltersPrivate(t *testing.T) {
	srv, _ := fakeDoH(t, "192.168.1.10")
	r := newTestResolver(srv.URL)

	if _, err := r.LookupPublic(context.Background(), "steamcommunity.com"); err == nil {
		t.Fatal("私网地址应被 LookupPublic 拒绝")
	}
	// Lookup 本身保留私网结果（直连场景可能合法）
	if _, err := r.Lookup(context.Background(), "steamcommunity.com"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
}

// TestPublicV4DropsIPv6AndPrivate 覆盖预热直连取址的口径：只保留公网 IPv4。
// IPv6 必须被滤掉——它会成为客户端首选，而我们没有干净来源校验 AAAA。
func TestPublicV4DropsIPv6AndPrivate(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("140.82.112.3"),        // 公网 v4：保留
		net.ParseIP("::ffff:140.82.113.3"), // 4-in-6：压成 v4 保留
		net.ParseIP("2606:50c0:8002::154"), // 纯 v6：滤掉
		net.ParseIP("192.168.1.10"),        // 私网：滤掉
		net.ParseIP("127.0.0.1"),           // 回环：滤掉
		net.ParseIP("::1"),                 // v6 回环：滤掉
		net.ParseIP("0.0.0.0"),             // 未指定：滤掉
	}
	got := publicV4(ips)
	if len(got) != 2 {
		t.Fatalf("期望保留 2 个公网 IPv4，实际 %d: %v", len(got), got)
	}
	if got[0].String() != "140.82.112.3" || got[1].String() != "140.82.113.3" {
		t.Fatalf("保留结果不对: %v", got)
	}
	for _, ip := range got {
		if ip.To4() == nil {
			t.Fatalf("不应保留 IPv6: %v", ip)
		}
	}
	if out := publicV4([]net.IP{net.ParseIP("2606:50c0:8002::154")}); len(out) != 0 {
		t.Fatalf("全 v6 输入应返回空: %v", out)
	}
}

func TestResolveSet(t *testing.T) {
	srv, _ := fakeDoH(t, "9.9.9.9")
	r := newTestResolver(srv.URL)

	got := r.ResolveSet(context.Background(), []string{"a.example.com", "b.example.com"}, 4)
	if len(got) != 2 {
		t.Fatalf("期望解析 2 个主机，实际 %d", len(got))
	}
	for h, ips := range got {
		if len(ips) == 0 || ips[0].String() != "9.9.9.9" {
			t.Errorf("%s 解析异常: %v", h, ips)
		}
	}
}

func TestHealthReporting(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)
	good, _ := fakeDoH(t, "3.3.3.3")
	r := newTestResolver(bad.URL, good.URL)

	if _, err := r.Lookup(context.Background(), "github.com"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	h := r.Health()
	if len(h) != 2 {
		t.Fatalf("期望2个端点，实际 %d", len(h))
	}
	var badOK, goodOK bool
	for _, item := range h {
		if item.Endpoint == bad.URL {
			badOK = item.OK
			if item.LastError == "" {
				t.Error("坏端点应有错误信息")
			}
		}
		if item.Endpoint == good.URL {
			goodOK = item.OK
		}
	}
	if badOK {
		t.Error("坏端点不应标记为健康")
	}
	if !goodOK {
		t.Error("好端点应标记为健康")
	}
}

// TestResetClearsCacheAndRestoresOrder 换网后要丢掉旧缓存与旧故障顺序，
// 让端点按配置顺序重新探测。
func TestResetClearsCacheAndRestoresOrder(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Status": 0,
			"Answer": []map[string]any{{
				"name": r.URL.Query().Get("name"), "type": 1, "TTL": 120, "data": "5.6.7.8",
			}},
		})
	}))
	t.Cleanup(good.Close)

	r := newTestResolver(bad.URL, good.URL)
	ctx := context.Background()
	if _, err := r.Lookup(ctx, "github.com"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := badHits.Load(); got != 1 {
		t.Fatalf("首次查询应访问坏端点 1 次，实际 %d", got)
	}
	if order := r.snapshotOrder(); order[0] != good.URL {
		t.Fatalf("失败端点应被降权到队尾: %v", order)
	}
	if _, err := r.Lookup(ctx, "github.com"); err != nil {
		t.Fatalf("二次 Lookup: %v", err)
	}
	if got := goodHits.Load(); got != 1 {
		t.Fatalf("缓存命中时不应再次请求，实际 %d 次", got)
	}

	r.Reset()

	if order := r.snapshotOrder(); order[0] != bad.URL || order[1] != good.URL {
		t.Fatalf("Reset 后端点顺序应恢复为配置顺序: %v", order)
	}
	for _, h := range r.Health() {
		if h.LastError != "" {
			t.Fatalf("Reset 后不应保留端点故障标记: %+v", h)
		}
	}
	if _, ok := r.fromCache("github.com"); ok {
		t.Fatal("Reset 后 DNS 缓存应为空")
	}

	if _, err := r.Lookup(ctx, "github.com"); err != nil {
		t.Fatalf("Reset 后 Lookup: %v", err)
	}
	if got := badHits.Load(); got != 2 {
		t.Fatalf("Reset 后应重新探测坏端点（实际 %d 次）", got)
	}
	if got := goodHits.Load(); got != 2 {
		t.Fatalf("Reset 后应重新请求好端点（实际 %d 次）", got)
	}
}

// TestGraceWindowAccountsLateFailures 覆盖「宽限期到点后在途结果仍要记账」。
//
// 背景：并查在首个成功响应后再等一个宽限期（lookupGraceWindow），到点就返回。
// 若此时把在途端点的结果一丢了之，晚到的失败永远不会 markFail，
// breaker（unionOrder 跳过冷却期内的失败端点）就永不生效——
// 于是每个域名的每次冷查询都要重付一个宽限期。
// 实测表现：tongtu test 里 15 个域名的 DNS 一列全是 ~2000ms，
// 而健康端点只要 30-150ms。
func TestGraceWindowAccountsLateFailures(t *testing.T) {
	good, _ := fakeDoH(t, "1.2.3.4")
	// 比宽限期更晚才失败的端点（复刻 1.1.1.1 在 2068ms 才报错的形态）。
	// 注意用比 newTestResolver 更大的 Timeout：否则查询会在宽限期之前就超时返回，
	// 走不到「晚到」这条路径。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(lookupGraceWindow + 700*time.Millisecond)
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	t.Cleanup(slow.Close)

	r := New(Config{
		Endpoints: []string{slow.URL, good.URL},
		Timeout:   5 * time.Second,
		CacheMin:  time.Second,
		CacheMax:  time.Minute,
	}, nil)

	ips := r.LookupAllUncached(context.Background(), "example.com")
	if len(ips) != 1 || ips[0].String() != "1.2.3.4" {
		t.Fatalf("应拿到健康端点的结果，实际 %v", ips)
	}

	// 慢端点的失败在宽限期之后才到达：必须被读走并记账（降权 + 进冷却）
	hasSlow := func() bool {
		for _, ep := range r.unionOrder() {
			if ep == slow.URL {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && hasSlow() {
		time.Sleep(50 * time.Millisecond)
	}
	if hasSlow() {
		t.Fatal("宽限期后到达的失败结果未被记账：慢端点仍在并查名单里，breaker 永不生效")
	}

	// breaker 生效后，第二次并查只查健康端点，不该再等满一个宽限期
	start := time.Now()
	ips2 := r.LookupAllUncached(context.Background(), "example.com")
	elapsed := time.Since(start)
	if len(ips2) != 1 || ips2[0].String() != "1.2.3.4" {
		t.Fatalf("第二次并查结果异常: %v", ips2)
	}
	if elapsed >= lookupGraceWindow {
		t.Fatalf("breaker 生效后第二次并查不应再等宽限期，实际耗时 %v", elapsed)
	}
}
