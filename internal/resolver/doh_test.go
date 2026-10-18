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

// TestPublicV4DropsIPv6AndPrivate 覆盖 hosts 模式取址的口径：只保留公网 IPv4。
// IPv6 必须被滤掉——写进 hosts 的地址会成为客户端首选，而我们没有干净来源校验 AAAA。
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
