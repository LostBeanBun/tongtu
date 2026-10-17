package resolver

import (
	"net"
	"testing"
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
