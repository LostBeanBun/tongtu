package tray

import (
	"strings"
	"testing"

	"tongtu/internal/app"
	"tongtu/internal/proxy"
)

// TestTooltipText 覆盖托盘悬停提示：不打开窗口也要能看出「在不在跑、接管了什么、走了多少流量」。
func TestTooltipText(t *testing.T) {
	running := app.Status{
		Running:     true,
		SystemProxy: true,
		Hosts:       false, // 未以管理员运行时 hosts 不生效，提示里要如实体现
		MITM:        true,
		Proxy:       proxy.Snapshot{BytesIn: 1572864, BytesOut: 348160},
	}
	got := tooltipText(running)
	for _, want := range []string{
		"通途",
		"运行中",
		"系统代理✓",
		"Hosts✗",
		"反代✓",
		"↓1.5M",
		"↑340K",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("提示里缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("托盘提示必须是单行（多行会被系统截断）: %q", got)
	}

	stopped := tooltipText(app.Status{})
	if !strings.Contains(stopped, "已停止") {
		t.Errorf("未运行时应写「已停止」: %s", stopped)
	}
	for _, want := range []string{"系统代理✗", "Hosts✗", "反代✗"} {
		if !strings.Contains(stopped, want) {
			t.Errorf("未运行时 %q 也要如实标出: %s", want, stopped)
		}
	}
}

// TestShortBytes 覆盖紧凑体积写法（托盘提示宽度是硬约束，单位不能太长）。
func TestShortBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1K"},
		{1536, "1.5K"},
		{340 * 1024, "340K"},
		{1 << 20, "1M"},
		{1572864, "1.5M"},
		{1 << 30, "1G"},
		{1 << 40, "1T"},
		{1 << 50, "1024T"}, // 超出 T 之后不再往上，避免出现 "1P" 这种没定义的单位
	}
	for _, c := range cases {
		if got := shortBytes(c.in); got != c.want {
			t.Errorf("shortBytes(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
