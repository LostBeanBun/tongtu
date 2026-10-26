package resolver

// remote.go 接入远程 hosts 数据源（GitHub520 等公开维护的优选 IP 列表）。
//
// 动机：GFW 对 GitHub/Steam 的 DNS 污染会使 DoH 返回假 IP 或不可达的边缘 IP；
// 社区维护的 hosts 列表提供实测可用的 IP，作为 DoH 之外的候选来源。
// 列表格式：每行 "IP 主机名"（兼容 hosts 文件的注释与空行）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type remoteHosts struct {
	urls   []string
	ttl    time.Duration
	client *http.Client
	log    *slog.Logger

	mu       sync.Mutex
	entries  map[string][]net.IP
	fetched  time.Time
	fetching bool
	// gen 在每次 invalidate 时递增：用于丢弃「换网前发出、换网后才回来」的旧抓取结果。
	gen uint64
}

func newRemoteHosts(urls []string, ttl time.Duration, client *http.Client, log *slog.Logger) *remoteHosts {
	if log == nil {
		log = slog.Default()
	}
	return &remoteHosts{urls: urls, ttl: ttl, client: client, log: log, entries: map[string][]net.IP{}}
}

// ips 返回指向 host 的远程候选 IP；数据为空或过期时触发后台刷新（不阻塞）。
func (rh *remoteHosts) ips(host string) []net.IP {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	if len(rh.urls) > 0 && !rh.fetching && (rh.fetched.IsZero() || time.Since(rh.fetched) > rh.ttl) {
		rh.fetching = true
		gen := rh.gen
		go rh.refresh(gen)
	}
	out := rh.entries[host]
	if len(out) == 0 {
		return nil
	}
	cp := make([]net.IP, len(out))
	copy(cp, out)
	return cp
}

// invalidate 丢弃当前远程 hosts 数据，并让下一次 ips 重新抓取。
//
// 换网后旧列表里的 IP 可能已经不可达；同时递增 gen，使仍在途的旧抓取结果被丢弃，
// 不会在换网之后又把旧数据写回来。
func (rh *remoteHosts) invalidate() {
	rh.mu.Lock()
	rh.gen++
	rh.entries = map[string][]net.IP{}
	rh.fetched = time.Time{}
	rh.fetching = false
	rh.mu.Unlock()
	rh.client.CloseIdleConnections()
}

func (rh *remoteHosts) refresh(gen uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	entries := make(map[string][]net.IP)
	got := 0
	for _, u := range rh.urls {
		data, err := rh.fetch(ctx, u)
		if err != nil {
			rh.log.Warn("远程 hosts 源获取失败", "url", u, "err", err)
			continue
		}
		n := parseHosts(data, entries)
		rh.log.Info("远程 hosts 源已加载", "url", u, "entries", n)
		got++
	}

	rh.mu.Lock()
	defer rh.mu.Unlock()
	if gen != rh.gen {
		// 本轮抓取已被 Reset 作废：丢弃结果；fetching 归新一轮抓取所有。
		return
	}
	rh.fetching = false
	if got == 0 {
		// 全部失败：延长下次重试间隔，避免频繁请求
		rh.fetched = time.Now().Add(-rh.ttl / 2)
		return
	}
	rh.entries = entries
	rh.fetched = time.Now()
}

func (rh *remoteHosts) fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tongtu/1.0")
	resp, err := rh.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// parseHosts 解析 hosts 格式文本，合并进 out，返回解析出的条目数。
func parseHosts(text string, out map[string][]net.IP) int {
	count := 0
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.To4() == nil || !validIP(ip) {
			continue
		}
		host := normalizeHost(fields[1])
		if host == "" {
			continue
		}
		if hasIP(out[host], ip.To4()) {
			continue
		}
		out[host] = append(out[host], ip.To4())
		count++
	}
	return count
}

func hasIP(ips []net.IP, ip net.IP) bool {
	for _, v := range ips {
		if v.Equal(ip) {
			return true
		}
	}
	return false
}
