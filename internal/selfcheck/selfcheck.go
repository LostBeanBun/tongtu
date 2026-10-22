// Package selfcheck 对「加速是否真的生效」做一次端到端体检。
//
// 每个域名依次回答四个问题：
//   - 干净解析能不能拿到 IP（走 DoH，绕开被污染的本地 DNS）
//   - 解析出来的 IP 上 TCP 443 通不通
//   - 带真 SNI 直连 HTTPS 通不通（反代名单里的域名，直连本来就该不通）
//   - 经通途本地代理请求 HTTPS 通不通
//
// 命令行（tongtu test）与窗口里的「自检」按钮共用这一份逻辑，只是呈现方式不同：
// 终端画表格，窗口把同样的结论写进日志卡片。因此这里只产出数据，不做任何排版。
package selfcheck

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tongtu/internal/resolver"
	"tongtu/internal/rules"
)

// DefaultHosts 是默认的体检域名：Steam 与 GitHub 及其主要附属站点。
var DefaultHosts = []string{
	"steamcommunity.com",
	"www.steamcommunity.com",
	"help.steamcommunity.com",
	"store.steampowered.com",
	"login.steampowered.com",
	"cdn.steamstatic.com",
	"avatars.steamstatic.com",
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"raw.githubusercontent.com",
	"objects.githubusercontent.com",
	"avatars.githubusercontent.com",
	"gist.githubusercontent.com",
	"gist.github.com",
}

// Verdict 是单个域名的体检结论。
type Verdict int

const (
	// VerdictOK：加速路径可用（经本地代理请求成功）。本地代理没在跑时，
	// 直连成功也算可用——此时没有加速可言，但站点本身是通的。
	VerdictOK Verdict = iota
	// VerdictDirectOnly：经代理不通，但直连能通。站点能用，但没被加速。
	VerdictDirectOnly
	// VerdictFail：两条路都不通（解析失败也算）。
	VerdictFail
)

// Word 返回结论的中文说法（命令行与窗口共用同一套措辞）。
func (v Verdict) Word() string {
	switch v {
	case VerdictOK:
		return "可用"
	case VerdictDirectOnly:
		return "仅直连"
	default:
		return "不通"
	}
}

// Row 是一个域名的体检结果（纯数据，便于单测与多处复用）。
type Row struct {
	Host     string
	IP       string // 干净解析出的首选 IP（有多个候选时形如 "1.2.3.4 +2"）
	DNSMs    int64
	DNSOK    bool
	TCP      string // "42ms" / "不可达"
	TCPOK    bool
	Direct   string // "HTTP 200" / "失败: 超时"
	DirectOK bool
	Proxy    string // 同上；未测时为空
	ProxyOK  bool
	// ProxyTested 报告「经代理」这一列是否真的测了（本地代理没跑时为 false）。
	ProxyTested bool
	MITM        string // "✓" / "–" / "未启用"
	Verdict     Verdict
}

// Describe 把一行的各列串成一句人话。
func (r Row) Describe() string {
	var parts []string
	if r.DNSOK {
		parts = append(parts, fmt.Sprintf("解析 %s（%dms）", r.IP, r.DNSMs))
		parts = append(parts, "TCP "+r.TCP)
	} else {
		parts = append(parts, "解析失败")
	}
	parts = append(parts, "直连 "+r.Direct)
	if r.ProxyTested {
		parts = append(parts, "经代理 "+r.Proxy)
	}
	if r.MITM != "" && r.MITM != "未启用" {
		parts = append(parts, "反代 "+r.MITM)
	}
	return strings.Join(parts, " · ")
}

// Summary 汇总一次体检：每种结论各多少个域名。
func Summary(rows []Row) string {
	if len(rows) == 0 {
		return "没有可体检的域名"
	}
	var ok, direct, fail, untested int
	for _, r := range rows {
		switch r.Verdict {
		case VerdictOK:
			ok++
		case VerdictDirectOnly:
			direct++
		default:
			fail++
		}
		if !r.ProxyTested {
			untested++
		}
	}
	s := fmt.Sprintf("%d 个域名：%d 可用", len(rows), ok)
	if direct > 0 {
		s += fmt.Sprintf(" · %d 仅直连", direct)
	}
	if fail > 0 {
		s += fmt.Sprintf(" · %d 不通", fail)
	}
	if untested == len(rows) {
		s += "（本地代理未运行，只测了直连）"
	}
	return s
}

// Prober 是一次体检所需的全部输入。
type Prober struct {
	Res          *resolver.Resolver
	Listen       string // 本地代理地址（经代理测试用）
	ProxyUp      bool   // 本地代理是否在运行
	MITMOn       bool   // 无 SNI 反代是否启用
	MITMPatterns []string
	Timeout      time.Duration // 单个域名的总超时（<=0 时用 20 秒）
}

// Run 逐个域名体检；ctx 取消时立刻收手（已测出的行照常返回）。
func (p *Prober) Run(ctx context.Context, hosts []string) []Row {
	rows := make([]Row, 0, len(hosts))
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		rows = append(rows, p.probe(ctx, h))
	}
	return rows
}

func (p *Prober) probe(ctx context.Context, host string) Row {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r := Row{Host: host, MITM: mitmMark(p.MITMOn, p.MITMPatterns, host)}

	t0 := time.Now()
	// 用 LookupAll：与代理一致（静态/远程优选 + 多端点并查，且不回退系统 DNS，
	// 避免在污染窗口下展示假 IP）。
	ips := p.Res.LookupAll(ctx, host)
	r.DNSMs = time.Since(t0).Milliseconds()
	if len(ips) == 0 {
		r.Direct, r.Proxy = "未测（解析失败）", "未测（解析失败）"
		r.Verdict = VerdictFail
		return r
	}
	r.DNSOK = true
	r.IP = ips[0].String()
	if len(ips) > 1 {
		r.IP = fmt.Sprintf("%s +%d", ips[0], len(ips)-1)
	}

	t1 := time.Now()
	if conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), "443")); err == nil {
		r.TCPOK = true
		r.TCP = fmt.Sprintf("%dms", time.Since(t1).Milliseconds())
		_ = conn.Close()
	} else {
		r.TCP = "不可达"
	}

	// 直连（真 SNI）能不能通：反代名单就是为「不通」的域名准备的。
	if s, err := httpsDirect(ctx, host); err != nil {
		r.Direct = "失败: " + shortErr(err)
	} else {
		r.DirectOK, r.Direct = true, s
	}
	if p.ProxyUp {
		r.ProxyTested = true
		if s, err := httpsViaProxy(ctx, p.Listen, host); err != nil {
			r.Proxy = "失败: " + shortErr(err)
		} else {
			r.ProxyOK, r.Proxy = true, s
		}
	}

	switch {
	case r.ProxyOK:
		r.Verdict = VerdictOK
	case !p.ProxyUp && r.DirectOK:
		r.Verdict = VerdictOK // 本地代理没跑：直连通就是「能用」，只是没加速
	case r.DirectOK:
		r.Verdict = VerdictDirectOnly
	default:
		r.Verdict = VerdictFail
	}
	return r
}

// mitmMark 标注该域名会不会走无 SNI 反代：✓ 走、– 直连（配置里没开反代时统一显示「未启用」）。
func mitmMark(mitmOn bool, patterns []string, host string) string {
	if !mitmOn {
		return "未启用"
	}
	if rules.MatchPatterns(host, patterns) {
		return "✓"
	}
	return "–"
}

// httpsDirect 直接（不经代理、带真 SNI）发一次 HTTPS 请求，成功返回 HTTP 状态描述。
func httpsDirect(ctx context.Context, host string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // 关键：绕开系统代理与本地代理，看真实直连结果
			TLSHandshakeTimeout: 8 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}
	return doHTTPS(ctx, client, host)
}

// httpsViaProxy 通过本地代理发起一次 HTTPS 请求，成功返回 HTTP 状态描述。
func httpsViaProxy(ctx context.Context, proxyAddr, host string) (string, error) {
	u, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(u),
			TLSHandshakeTimeout: 8 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}
	return doHTTPS(ctx, client, host)
}

func doHTTPS(ctx context.Context, client *http.Client, host string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}

func shortErr(err error) string {
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 48 {
		s = s[:48] + "…"
	}
	return s
}
