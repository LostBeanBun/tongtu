package resolver

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseHosts(t *testing.T) {
	text := `# GitHub520 Host Start
140.82.112.3                  github.com
140.82.113.4                  github.com
20.205.243.166                gist.github.com   # 注释

not-an-ip                     example.com
1.2.3.4                       # 缺主机名
::1                           ipv6.example.com
`
	entries := map[string][]net.IP{}
	n := parseHosts(text, entries)
	if n != 3 {
		t.Fatalf("条目数 = %d, 期望 3", n)
	}
	if got := entries["github.com"]; len(got) != 2 {
		t.Fatalf("github.com IP 数 = %d, 期望 2", len(got))
	}
	if got := entries["gist.github.com"]; len(got) != 1 || got[0].String() != "20.205.243.166" {
		t.Fatalf("gist.github.com = %v, 期望 [20.205.243.166]", got)
	}
	if _, ok := entries["example.com"]; ok {
		t.Fatal("非法 IP 不应被解析")
	}
	if _, ok := entries["ipv6.example.com"]; ok {
		t.Fatal("IPv6 应被跳过（仅取 A 记录）")
	}
}

func TestParseHostsDedup(t *testing.T) {
	text := "140.82.112.3 github.com\n140.82.112.3 github.com\n"
	entries := map[string][]net.IP{}
	parseHosts(text, entries)
	if got := entries["github.com"]; len(got) != 1 {
		t.Fatalf("去重后 IP 数 = %d, 期望 1", len(got))
	}
}

// TestRemoteHostsInvalidateRefetches 换网后远程 hosts 必须重新抓取，不能继续用旧 IP。
func TestRemoteHostsInvalidateRefetches(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "140.82.112.3 github.com\n")
	}))
	t.Cleanup(srv.Close)

	rh := newRemoteHosts([]string{srv.URL}, time.Minute, srv.Client(), nil)
	if got := rh.ips("github.com"); len(got) != 0 {
		t.Fatalf("首次调用只应触发后台抓取，实际已返回 %v", got)
	}
	waitRemoteIPs(t, rh, "github.com")

	rh.invalidate()
	if got := rh.ips("github.com"); len(got) != 0 {
		t.Fatalf("invalidate 后应立即丢弃旧数据，实际仍返回 %v", got)
	}
	waitRemoteIPs(t, rh, "github.com")
	if got := hits.Load(); got < 2 {
		t.Fatalf("invalidate 后应重新抓取，实际只请求 %d 次", got)
	}
}

// waitRemoteIPs 等待远程 hosts 的后台抓取完成。
func waitRemoteIPs(t *testing.T, rh *remoteHosts, host string) []net.IP {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ips := rh.ips(host); len(ips) > 0 {
			return ips
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待远程 hosts 抓取超时（%s）", host)
	return nil
}
