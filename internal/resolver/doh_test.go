package resolver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
// IPv6 必须被滤掉——预热 / 批量解析统一用 v4（本机 v6 出口常不可用）；
// 请求路径需要 v6 时走 queryBest 的 AAAA 兜底，不经过这里。
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

// fakeDoHTyped 启动按 type 参数分类应答的 DoH 模拟服务：
// A 查询返回 a、AAAA 查询返回 aaaa（均为 data 列表，空表示 0 条应答），
// status 为响应的 DNS rcode（0 = NOERROR、2 = SERVFAIL、3 = NXDOMAIN…）。
func fakeDoHTyped(t *testing.T, status int, a, aaaa []string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var data []string
		var rr int
		switch r.URL.Query().Get("type") {
		case "A":
			data, rr = a, 1
		case "AAAA":
			data, rr = aaaa, 28
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		answers := make([]map[string]any, 0, len(data))
		for _, d := range data {
			answers = append(answers, map[string]any{"name": name, "type": rr, "TTL": 120, "data": d})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Status": status, "Answer": answers})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestNODATAFallsBackToAAAA 覆盖「只有 AAAA 记录的域名」：A 查询正常应答但 0 条
// 记录（NODATA）时，对同一端点继续查 AAAA，拿到干净的 IPv6 候选；
// 端点全程保持健康，第二次查询命中缓存不再打上游。
func TestNODATAFallsBackToAAAA(t *testing.T) {
	const v6 = "2606:50c0:8002::154"
	srv, hits := fakeDoHTyped(t, 0, nil, []string{v6})
	r := newTestResolver(srv.URL)
	ctx := context.Background()

	ips, err := r.Lookup(ctx, "v6only.example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != v6 {
		t.Fatalf("应拿到 AAAA 兜底结果，实际 %v", ips)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("应先查 A 再查 AAAA（2 次上游请求），实际 %d 次", n)
	}
	for _, h := range r.Health() {
		if !h.OK {
			t.Fatalf("端点应保持健康（NODATA 是回答不是故障）: %+v", h)
		}
	}
	// 二次查询命中缓存
	if _, err := r.Lookup(ctx, "v6only.example.com"); err != nil {
		t.Fatalf("二次 Lookup: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("缓存命中后不应再请求上游，实际 %d 次", n)
	}
	// 预热 / 批量解析的取址口径仍是 IPv4：v6-only 域名在这里应明确失败，而不是静默拿到 v6。
	if _, err := r.LookupPublic(ctx, "v6only.example.com"); err == nil {
		t.Fatal("LookupPublic 应拒绝只有 IPv6 的域名（预热口径只认 IPv4）")
	}
}

// TestNXDOMAINIsAnswerNotFailure 覆盖 NXDOMAIN 的分类：端点正常应答「域名不存在」，
// 端点必须保持健康、不进熔断冷却，且 NXDOMAIN 之后不再多查一次 AAAA。
func TestNXDOMAINIsAnswerNotFailure(t *testing.T) {
	srv, hits := fakeDoHTyped(t, 3, nil, nil)
	r := newTestResolver(srv.URL)

	if ips := r.LookupAllUncached(context.Background(), "nope.example.com"); ips != nil {
		t.Fatalf("不存在的域名不应返回候选，实际 %v", ips)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("NXDOMAIN 后不应再查 AAAA，期望 1 次请求，实际 %d 次", n)
	}
	for _, h := range r.Health() {
		if !h.OK || h.LastError != "" {
			t.Fatalf("NXDOMAIN 后端点应保持健康: %+v", h)
		}
	}
}

// TestNXDOMAINDoesNotDemoteEndpoint 覆盖顺序 Lookup 路径：第一个端点答 NXDOMAIN、
// 第二个端点有 A 记录时，解析应成功，且 NXDOMAIN 的端点**不被降权**（不挪到队尾）。
// 此前 NXDOMAIN 会走 markFail：一个不存在的域名就能把健康端点降权一个冷却期。
func TestNXDOMAINDoesNotDemoteEndpoint(t *testing.T) {
	nx, _ := fakeDoHTyped(t, 3, nil, nil)
	good, _ := fakeDoH(t, "7.7.7.7")
	r := newTestResolver(nx.URL, good.URL)

	ips, err := r.Lookup(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "7.7.7.7" {
		t.Fatalf("应拿到健康端点的结果，实际 %v", ips)
	}
	order := r.snapshotOrder()
	if order[0] != nx.URL {
		t.Fatalf("NXDOMAIN 端点不应被降权到队尾: %v", order)
	}
	for _, h := range r.Health() {
		if h.Endpoint == nx.URL && (!h.OK || h.LastError != "") {
			t.Fatalf("NXDOMAIN 端点应保持健康: %+v", h)
		}
	}
}

// TestSERVFAILStillMarksEndpointFailure 固定分类的另一半：rcode 2（SERVFAIL）
// 仍是端点故障——它表示端点（或其上游）没能完成解析，应照旧降权进冷却。
func TestSERVFAILStillMarksEndpointFailure(t *testing.T) {
	bad, _ := fakeDoHTyped(t, 2, nil, nil)
	good, _ := fakeDoH(t, "8.8.4.4")
	r := newTestResolver(bad.URL, good.URL)

	ips, err := r.Lookup(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "8.8.4.4" {
		t.Fatalf("应拿到健康端点的结果，实际 %v", ips)
	}
	order := r.snapshotOrder()
	if order[0] != good.URL {
		t.Fatalf("SERVFAIL 端点应被降权到队尾: %v", order)
	}
	for _, h := range r.Health() {
		if h.Endpoint == bad.URL {
			if h.OK {
				t.Fatalf("SERVFAIL 端点不应标记为健康: %+v", h)
			}
			if !strings.Contains(h.LastError, "rcode 2") {
				t.Fatalf("错误信息应保留 rcode: %+v", h)
			}
		}
	}
}

// TestNoRecordsOnBothTypesIsNotFailure 覆盖「A 与 AAAA 都没有记录」：
// 两次查询都是正常应答，端点保持健康、不进冷却（LookupAll 路径没有系统回退，
// 返回空即到此为止）。
func TestNoRecordsOnBothTypesIsNotFailure(t *testing.T) {
	srv, hits := fakeDoHTyped(t, 0, nil, nil)
	r := newTestResolver(srv.URL)

	if ips := r.LookupAllUncached(context.Background(), "empty.example.com"); ips != nil {
		t.Fatalf("无记录的域名不应返回候选，实际 %v", ips)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("应先查 A 再查 AAAA（2 次上游请求），实际 %d 次", n)
	}
	for _, h := range r.Health() {
		if !h.OK {
			t.Fatalf("端点应保持健康: %+v", h)
		}
	}
}
