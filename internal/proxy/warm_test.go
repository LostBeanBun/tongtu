package proxy

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
)

// TestWarmPrefillsResolver 验证预热会走多端点干净解析（LookupAll），
// 使第一个真实请求直接命中缓存。
func TestWarmPrefillsResolver(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "预热应走多端点解析"), discardLog())
	var lookups int32
	p.SetLookupAll(func(context.Context, string) []net.IP {
		atomic.AddInt32(&lookups, 1)
		return []net.IP{net.IPv4(127, 0, 0, 1)}
	})

	p.Warm(context.Background(), []string{"steamcommunity.com"}, 1)

	if atomic.LoadInt32(&lookups) == 0 {
		t.Fatal("预热应触发干净解析")
	}
}

// TestWarmHonorsCancel ctx 取消后立即返回，不阻塞退出流程。
func TestWarmHonorsCancel(t *testing.T) {
	p := NewWithLookup(mustRules(t), failLookup(t, "已取消的预热不应解析"), discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Warm(ctx, []string{"steamcommunity.com", "github.com"}, 2)
}
