// package resolver 实现带缓存与端点故障转移的 DoH (DNS over HTTPS) 解析器。
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config 控制 DoH 解析行为。
type Config struct {
	Endpoints []string
	Timeout   time.Duration
	CacheMin  time.Duration
	CacheMax  time.Duration

	// RemoteHosts 是远程优选 IP 列表（hosts 格式）来源，作为 DoH 之外的候选人。
	RemoteHosts    []string
	RemoteHostsTTL time.Duration
	// StaticHosts 是静态优选 IP（host -> IP 字符串列表），优先级最高。
	StaticHosts map[string][]string
}

// Health 描述单个 DoH 端点的健康状态。
type Health struct {
	Endpoint  string    `json:"endpoint"`
	OK        bool      `json:"ok"`
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

type endpointState struct {
	lastOK  time.Time
	lastTry time.Time
	err     error
}

type cacheEntry struct {
	ips []net.IP
	exp time.Time
}

type dohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  uint32 `json:"TTL"`
	Data string `json:"data"`
}

type dohMessage struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// Resolver 按端点顺序轮询 DoH 服务，失败端点自动降权，全部失败时回退系统 DNS。
type Resolver struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	remote *remoteHosts
	static map[string][]net.IP

	mu    sync.Mutex
	cache map[string]cacheEntry
	order []string // 端点故障转移顺序
	state map[string]endpointState
}

// New 创建 Resolver。
func New(cfg Config, log *slog.Logger) *Resolver {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.CacheMin <= 0 {
		cfg.CacheMin = time.Minute
	}
	if cfg.CacheMax <= 0 || cfg.CacheMax < cfg.CacheMin {
		cfg.CacheMax = 10 * time.Minute
	}
	if cfg.RemoteHostsTTL <= 0 {
		cfg.RemoteHostsTTL = 30 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	var remote *remoteHosts
	if len(cfg.RemoteHosts) > 0 {
		remote = newRemoteHosts(cfg.RemoteHosts, cfg.RemoteHostsTTL, &http.Client{Timeout: 15 * time.Second}, log)
	}
	static := make(map[string][]net.IP, len(cfg.StaticHosts))
	for h, raw := range cfg.StaticHosts {
		host := normalizeHost(h)
		if host == "" {
			continue
		}
		for _, s := range raw {
			ip := net.ParseIP(strings.TrimSpace(s))
			if ip == nil || ip.To4() == nil || !validIP(ip) {
				continue
			}
			static[host] = append(static[host], ip.To4())
		}
	}
	return &Resolver{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log,
		remote: remote,
		static: static,
		cache:  make(map[string]cacheEntry),
		order:  append([]string(nil), cfg.Endpoints...),
		state:  make(map[string]endpointState),
	}
}

// mergeRemote 把静态/远程优选 IP 前置到候选列表（静态 > 远程 > DoH）。
func (r *Resolver) mergeRemote(host string, out []net.IP) []net.IP {
	var extra []net.IP
	extra = append(extra, r.static[host]...)
	if r.remote != nil {
		extra = append(extra, r.remote.ips(host)...)
	}
	if len(extra) == 0 {
		return out
	}
	seen := make(map[string]bool, len(extra)+len(out))
	merged := make([]net.IP, 0, len(extra)+len(out))
	for _, ip := range extra {
		k := ip.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, ip)
	}
	for _, ip := range out {
		k := ip.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, ip)
	}
	return merged
}

// Lookup 查询 host 的 A 记录（IPv4），带缓存；DoH 全部失败时回退系统 DNS。
// 回退可以用到 IPv6 地址（只有 AAAA 的域名在直连路径上仍然可用）。
func (r *Resolver) Lookup(ctx context.Context, host string) ([]net.IP, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("空主机名")
	}
	if ips, ok := r.fromCache(host); ok {
		return ips, nil
	}

	var errs []string
	for _, ep := range r.unionOrder() {
		ips, ttl, err := r.query(ctx, ep, host)
		if err == nil && len(ips) == 0 {
			err = fmt.Errorf("无 A 记录")
		}
		if err != nil {
			r.markFail(ep, err)
			errs = append(errs, fmt.Sprintf("%s: %v", ep, err))
			continue
		}
		r.markOK(ep)
		r.store(host, ips, ttl)
		return ips, nil
	}

	// 全部 DoH 端点失败：回退系统 DNS，保证可用性优先（回退结果不写缓存）。
	// 网络参数用 "ip"（IPv4+IPv6）：只有 AAAA 的域名也能拿到可用地址走 IPv6 直连。
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DoH 解析失败且系统 DNS 失败 (%s): %w", strings.Join(errs, "; "), err)
	}
	r.log.Warn("DoH 端点全部失败，已回退系统 DNS", "host", host, "errors", errs)
	return ips, nil
}

// LookupPublic 与 Lookup 相同，但只保留公网 IPv4（供 hosts 模式使用）。
//
// 为什么把 IPv6 也滤掉：写进 hosts 的地址会成为客户端的**首选**（IPv6 优先），
// 而我们没有干净来源可以校验 AAAA（DoH 查询只取 A 记录）；写错一个不可达/被污染的
// 地址比不写更糟。反代域名走另一条路——那里写的是本机回环 `::1`（见 hosts.SetLocalIPv6）。
func (r *Resolver) LookupPublic(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := r.Lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	out := publicV4(ips)
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 未解析到公网 IPv4 地址", host)
	}
	return out, nil
}

// publicV4 只保留公网 IPv4：滤掉 IPv6（含 4-in-6）、私网、回环与未指定地址。
func publicV4(ips []net.IP) []net.IP {
	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		out = append(out, v4)
	}
	return out
}

// LookupAll 并查全部 DoH 端点并合并去重，返回干净候选 IP（命中缓存时直接返回）。
// 与 Lookup 不同，它刻意不回退系统 DNS——供「无 SNI 拨号」使用：
// 系统 DNS 在污染域名上会返回假 IP，宁可无候选也不能用。
func (r *Resolver) LookupAll(ctx context.Context, host string) []net.IP {
	host = normalizeHost(host)
	if host == "" {
		return nil
	}
	if ips, ok := r.fromCache(host); ok && len(ips) > 0 {
		return r.mergeRemote(host, ips)
	}
	ips := r.LookupAllUncached(ctx, host)
	if len(ips) > 0 {
		r.store(host, ips, r.cfg.CacheMin)
	}
	return ips
}

// lookupGraceWindow 是「并查」在首个成功响应后继续等待其它端点的时间上限。
// 慢或不可达的端点不再拖住整体延迟，同时仍给其它端点（可能更干净）一点时间。
const lookupGraceWindow = 2 * time.Second

// LookupAllUncached 并查全部端点并合并去重（忽略缓存，命中缓存时也强制重查）。
// 用于优选 IP 探测：需要真实可用的候选，而非可能过期的缓存。
func (r *Resolver) LookupAllUncached(ctx context.Context, host string) []net.IP {
	type result struct {
		ep  string
		ips []net.IP
		ttl time.Duration
		err error
	}
	order := r.unionOrder()
	resCh := make(chan result, len(order))
	for _, ep := range order {
		go func(ep string) {
			ips, ttl, err := r.query(ctx, ep, host)
			resCh <- result{ep: ep, ips: ips, ttl: ttl, err: err}
		}(ep)
	}
	seen := make(map[string]bool, len(order))
	var out []net.IP
	var minTTL time.Duration

	// 首个成功响应后开始「宽限计时」：慢/不可达端点（如被拒连的 1.1.1.1）
	// 不再拖住整体延迟，但仍在窗口内收集其它端点的结果以对抗单点污染。
	var graceC <-chan time.Time
	var graceTimer *time.Timer
	collect := func(res result) {
		if res.err != nil || len(res.ips) == 0 {
			if res.err == nil {
				res.err = fmt.Errorf("无 A 记录")
			}
			// 调用方取消导致的失败不视为端点故障，避免误降权
			if ctx.Err() == nil {
				r.markFail(res.ep, res.err)
			}
			return
		}
		r.markOK(res.ep)
		for _, ip := range res.ips {
			key := ip.String()
			if seen[key] || !validIP(ip) {
				continue
			}
			seen[key] = true
			out = append(out, ip)
		}
		if res.ttl > 0 && (minTTL == 0 || res.ttl < minTTL) {
			minTTL = res.ttl
		}
		if graceC == nil {
			graceTimer = time.NewTimer(lookupGraceWindow)
			graceC = graceTimer.C
		}
	}

	for i := 0; i < len(order); i++ {
		if graceC == nil {
			collect(<-resCh)
			continue
		}
		select {
		case res := <-resCh:
			collect(res)
		case <-graceC:
			i = len(order) // 宽限期到，用已有结果
		}
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}
	out = r.mergeRemote(host, out)
	if len(out) == 0 {
		return nil
	}
	r.store(host, out, minTTL)
	return out
}

// ResolveSet 并发解析一组主机名，返回 host -> 公网 IP 列表（解析失败的键不出现）。
func (r *Resolver) ResolveSet(ctx context.Context, hosts []string, workers int) map[string][]net.IP {
	if workers <= 0 {
		workers = 8
	}
	out := make(map[string][]net.IP, len(hosts))
	var mu sync.Mutex
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ips, err := r.LookupPublic(ctx, h)
			if err != nil || len(ips) == 0 {
				r.log.Warn("解析失败", "host", h, "err", err)
				return
			}
			mu.Lock()
			out[h] = ips
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}

// Health 返回各 DoH 端点健康状态（按当前轮询顺序）。
func (r *Resolver) Health() []Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Health, 0, len(r.order))
	for _, ep := range r.order {
		st := r.state[ep]
		h := Health{Endpoint: ep, OK: st.err == nil && !st.lastOK.IsZero(), LastOK: st.lastOK}
		if st.err != nil {
			h.LastError = st.err.Error()
		}
		out = append(out, h)
	}
	return out
}

// query 执行一次 DoH 查询，返回 IPv4 地址与建议缓存时长。
func (r *Resolver) query(ctx context.Context, endpoint, host string) ([]net.IP, time.Duration, error) {
	u := endpoint + "?name=" + url.QueryEscape(host) + "&type=A"
	qctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, 0, err
	}
	var msg dohMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, 0, fmt.Errorf("响应解析失败: %w", err)
	}
	if msg.Status != 0 {
		return nil, 0, fmt.Errorf("DNS rcode %d", msg.Status)
	}
	var ips []net.IP
	var minTTL time.Duration
	for _, a := range msg.Answer {
		if a.Type != 1 { // 只取 A 记录
			continue
		}
		ip := net.ParseIP(a.Data)
		if !validIP(ip) {
			continue
		}
		ips = append(ips, ip.To4())
		ttl := time.Duration(a.TTL) * time.Second
		if ttl > 0 && (minTTL == 0 || ttl < minTTL) {
			minTTL = ttl
		}
	}
	if minTTL <= 0 {
		minTTL = r.cfg.CacheMin
	}
	if minTTL < r.cfg.CacheMin {
		minTTL = r.cfg.CacheMin
	}
	if minTTL > r.cfg.CacheMax {
		minTTL = r.cfg.CacheMax
	}
	return ips, minTTL, nil
}

func (r *Resolver) fromCache(host string) ([]net.IP, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[host]
	if !ok || !e.exp.After(time.Now()) {
		return nil, false
	}
	out := make([]net.IP, len(e.ips))
	copy(out, e.ips)
	return out, true
}

func (r *Resolver) store(host string, ips []net.IP, ttl time.Duration) {
	cp := make([]net.IP, len(ips))
	copy(cp, ips)
	r.mu.Lock()
	r.cache[host] = cacheEntry{ips: cp, exp: time.Now().Add(ttl)}
	r.mu.Unlock()
}

func (r *Resolver) snapshotOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *Resolver) markFail(endpoint string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state[endpoint] = endpointState{err: err, lastTry: time.Now()}
	// 失败端点移到队尾，让下次查询优先使用健康端点
	for i, v := range r.order {
		if v == endpoint && i < len(r.order)-1 {
			copy(r.order[i:], r.order[i+1:])
			r.order[len(r.order)-1] = endpoint
			break
		}
	}
}

func (r *Resolver) markOK(endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state[endpoint] = endpointState{lastOK: time.Now(), lastTry: time.Now()}
}

// breakerCooldown 是端点失败后的冷却时间：冷却期内不再参与「并查」，
// 避免不可达端点（如境内被拒连的 1.1.1.1）每次都把查询拖满宽限窗口。
const breakerCooldown = 90 * time.Second

// unionOrder 返回「并查」要查询的端点：跳过冷却期内失败的端点；
// 若全部处于冷却则退化为全部端点，保证仍有机会恢复。
func (r *Resolver) unionOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := append([]string(nil), r.order...)
	now := time.Now()
	healthy := make([]string, 0, len(all))
	for _, ep := range all {
		st := r.state[ep]
		if st.err != nil && !st.lastTry.IsZero() && now.Sub(st.lastTry) < breakerCooldown {
			continue
		}
		healthy = append(healthy, ep)
	}
	if len(healthy) == 0 {
		return all
	}
	return healthy
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

// validIP 过滤掉环回、未指定、组播与链路本地地址。
func validIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
}
