// noSNI 探针（tools/nosni-probe）：独立复验「无 SNI 反代」的出站路径是否仍然有效。
//
// 出站动作 = 与目标建 TCP → TLS 握手**不带 SNI**（阻断设备看不到被封的域名）→
// 在这条 TLS 里直接发 HTTP/1.1 请求，用 Host 头完成站点路由（证书不校验，
// 因为站点不知道该给哪张证书；真正的校验发生在客户端那一侧，由本地 CA 承担，
// 即 internal/mitm 与 internal/proxy/mitm.go 负责的那一半）。
//
// 本探针回答一个问题：**不经通途、裸的无 SNI 出站，今天还通吗？**
// 若这里能拿到 HTTP 200，说明 SNI 阻断依旧、无 SNI 出站依旧有效——
// 反代这条路径成立的前提没有变。
//
// 用法：
//
//	nosni-probe                      # 跑内置清单（反代出站路径 + 对照组）
//	nosni-probe <host> <ip>          # 单目标模式，退出码 0=拿到站点响应，1=失败
//
// 单目标模式供 tools\accel-matrix.ps1 的 D 路调用。
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type target struct {
	host string
	ip   string
	path string
}

func short(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 72 {
		s = s[:72] + "…"
	}
	return s
}

// probe 对单个 host+ip 走「无 SNI 出站 + Host 路由」，返回是否拿到站点响应。
func probe(t target, timeout time.Duration) bool {
	d := &net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", net.JoinHostPort(t.ip, "443"))
	if err != nil {
		fmt.Printf("  %-26s %-16s TCP失败: %v\n", t.host, t.ip, short(err))
		return false
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))

	// 出站 TLS：刻意不带 ServerName → 不发 SNI
	c := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true, // 站点不认得这个握手，默认证书；校验不在此层做
		MinVersion:         tls.VersionTLS12,
	})
	if err := c.Handshake(); err != nil {
		fmt.Printf("  %-26s %-16s 无SNI握手失败: %v\n", t.host, t.ip, short(err))
		return false
	}
	st := c.ConnectionState()
	_ = c.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\nAccept: */*\r\nConnection: close\r\n\r\n", t.path, t.host)
	if _, err := io.WriteString(c, req); err != nil {
		fmt.Printf("  %-26s %-16s 无SNI握手OK · 发请求失败: %v\n", t.host, t.ip, short(err))
		return false
	}
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil && n == 0 {
		fmt.Printf("  %-26s %-16s 无SNI握手OK · 读响应失败: %v\n", t.host, t.ip, short(err))
		return false
	}
	resp := string(buf[:n])
	line := resp
	if i := strings.Index(resp, "\r\n"); i > 0 {
		line = resp[:i]
	}
	// 关键判定：是否拿到了站点自己的响应
	ok := strings.HasPrefix(line, "HTTP/1.") && (strings.Contains(line, " 200") || strings.Contains(line, " 301") ||
		strings.Contains(line, " 302") || strings.Contains(line, " 404"))
	mark := "✅ 拿到站点响应"
	if !ok {
		mark = "⚠️ 响应异常"
	}
	fmt.Printf("  %-26s %-16s %s | %s | 外层证书=%s\n", t.host, t.ip, mark, line, st.PeerCertificates[0].Subject.CommonName)
	_ = c.Close()
	return ok
}

func main() {
	// 单目标模式：nosni-probe <host> <ip> —— 退出码 0=拿到站点响应，1=失败。
	if len(os.Args) == 3 {
		if probe(target{host: os.Args[1], ip: os.Args[2], path: "/"}, 8*time.Second) {
			os.Exit(0)
		}
		os.Exit(1)
	}
	fmt.Println("A. 被删能力复刻：无 SNI 出站 + Host 路由（0.11 反代的出站半边）")
	for _, t := range []target{
		{"steamcommunity.com", "184.30.89.225", "/"},
		{"steamcommunity.com", "104.89.103.51", "/"},
		{"www.steamcommunity.com", "104.89.103.51", "/"},
		{"raw.githubusercontent.com", "185.199.108.133", "/"},
		{"raw.githubusercontent.com", "185.199.110.133", "/"},
		{"avatars.steamstatic.com", "146.75.47.52", "/"},
		{"gist.github.com", "140.82.112.3", "/"},
	} {
		probe(t, 8*time.Second)
	}
	fmt.Println()
	fmt.Println("B. 对照：本来就能通的（验证探针自身不制造假阳性）")
	for _, t := range []target{
		{"github.com", "140.82.112.3", "/"},
		{"cdn.steamstatic.com", "146.75.47.52", "/"},
		{"store.steampowered.com", "23.15.142.182", "/"},
	} {
		probe(t, 8*time.Second)
	}
}
