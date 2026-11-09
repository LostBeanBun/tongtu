package selfcheck

import (
	"context"
	"strings"
	"testing"
)

func TestVerdictWord(t *testing.T) {
	cases := map[Verdict]string{VerdictOK: "可用", VerdictDirectOnly: "仅直连", VerdictFail: "不通"}
	for v, want := range cases {
		if got := v.Word(); got != want {
			t.Errorf("Verdict(%d).Word() = %q，期望 %q", v, got, want)
		}
	}
}

func TestSummaryCounts(t *testing.T) {
	rows := []Row{
		{Host: "a", Verdict: VerdictOK, ProxyTested: true},
		{Host: "b", Verdict: VerdictOK, ProxyTested: true},
		{Host: "c", Verdict: VerdictDirectOnly, ProxyTested: true},
		{Host: "d", Verdict: VerdictFail, ProxyTested: true},
	}
	got := Summary(rows)
	for _, want := range []string{"4 个域名", "2 可用", "1 仅直连", "1 不通"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q，缺少 %q", got, want)
		}
	}
}

func TestSummaryNotesProxyDown(t *testing.T) {
	rows := []Row{{Host: "a", Verdict: VerdictOK}, {Host: "b", Verdict: VerdictOK}}
	got := Summary(rows)
	if !strings.Contains(got, "本地代理未运行") {
		t.Errorf("本地代理没跑时摘要要说清楚，实际 %q", got)
	}
	if strings.Contains(got, "仅直连") || strings.Contains(got, "不通") {
		t.Errorf("没有这两类结果时不该出现它们的计数，实际 %q", got)
	}
}

func TestSummaryEmpty(t *testing.T) {
	if got := Summary(nil); !strings.Contains(got, "没有") {
		t.Errorf("空结果要有明确说法，实际 %q", got)
	}
}

func TestRowDescribe(t *testing.T) {
	r := Row{
		Host: "steamcommunity.com", IP: "104.89.103.51 +1", DNSMs: 42, DNSOK: true,
		TCP: "38ms", TCPOK: true,
		Direct: "失败: 超时", Proxy: "HTTP 200", ProxyOK: true, ProxyTested: true,
		Verdict: VerdictOK,
	}
	got := r.Describe()
	for _, want := range []string{"解析 104.89.103.51 +1（42ms）", "TCP 38ms", "直连 失败: 超时", "经代理 HTTP 200"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q，缺少 %q", got, want)
		}
	}
}

func TestRowDescribeDNSFailure(t *testing.T) {
	r := Row{Host: "raw.githubusercontent.com", Direct: "未测（解析失败）", Verdict: VerdictFail}
	got := r.Describe()
	if !strings.Contains(got, "解析失败") {
		t.Errorf("解析失败要说清楚，实际 %q", got)
	}
	if strings.Contains(got, "TCP") {
		t.Errorf("没解析出 IP 时不该假装测过 TCP，实际 %q", got)
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	p := &Prober{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows := p.Run(ctx, DefaultHosts); len(rows) != 0 {
		t.Errorf("ctx 已取消时不该继续体检，实际 %d 行", len(rows))
	}
}
