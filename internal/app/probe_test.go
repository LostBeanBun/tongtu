package app

// App.Probe 是窗口「诊断」面板的数据入口：只复用当前实例的解析器与运行状态。

import (
	"context"
	"testing"

	"tongtu/internal/config"
	"tongtu/internal/selfcheck"
)

// TestProbeCanceledContextReturnsRow ctx 已取消时 Prober 不会开始探测，
// App.Probe 仍要返回一行明确失败（界面不必特判 nil / 空结果）。
func TestProbeCanceledContextReturnsRow(t *testing.T) {
	a := &App{cfg: config.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	row := a.Probe(ctx, "github.com")
	if row.Host != "github.com" {
		t.Errorf("Host = %q，期望 github.com", row.Host)
	}
	if row.Verdict != selfcheck.VerdictFail {
		t.Errorf("取消后应返回「不通」，实际 %v", row.Verdict)
	}
}
