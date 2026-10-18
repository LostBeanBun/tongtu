package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"tongtu/internal/mitm"
)

// TestWarmPrefillsGoodIP 验证预热会把反代域名的优选 IP 写进缓存，
// 使第一个真实请求直接走快路径（不再需要候选竞速）。
func TestWarmPrefillsGoodIP(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	up.StartTLS()
	defer up.Close()
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := mitm.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	p := NewWithLookup(mustRules(t), failLookup(t, "预热不应走白名单解析"), discardLog())
	p.allowLoopback = true
	p.warmPort = u.Port() // 测试用随机端口；生产恒为 443
	var lookups int32
	p.SetLookupAll(func(context.Context, string) []net.IP {
		atomic.AddInt32(&lookups, 1)
		return []net.IP{net.IPv4(127, 0, 0, 1)}
	})
	p.EnableMITM(ca, func(h string) bool { return h == "steamcommunity.com" })

	p.Warm(context.Background(), []string{"steamcommunity.com"}, 1)

	ip, ok := p.goodIP("steamcommunity.com")
	if !ok {
		t.Fatal("预热后应写入优选 IP")
	}
	if !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("优选 IP 错误: %v", ip)
	}
	if atomic.LoadInt32(&lookups) == 0 {
		t.Fatal("预热反代域名应触发候选解析")
	}
}

// TestWarmNonMITMHostWarmsResolver 非反代主机只预热干净解析缓存，不写优选缓存。
func TestWarmNonMITMHostWarmsResolver(t *testing.T) {
	var calls int32
	p := NewWithLookup(mustRules(t), func(ctx context.Context, host string) ([]net.IP, error) {
		atomic.AddInt32(&calls, 1)
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}, discardLog())

	p.Warm(context.Background(), []string{"steamcommunity.com"}, 1)

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("非反代主机应预热解析缓存")
	}
	if _, ok := p.goodIP("steamcommunity.com"); ok {
		t.Fatal("非反代主机不应写入优选缓存")
	}
}

// TestWarmHonorsCancel ctx 取消后立即返回，不阻塞退出流程。
func TestWarmHonorsCancel(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "已取消的预热不应解析"), discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Warm(ctx, []string{"steamcommunity.com", "github.com"}, 2)
}
