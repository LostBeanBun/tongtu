package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 日志窗口的显示层测试：排版规则（时间/级别/属性）、配色语义、摘要渲染。
// 这些都只依赖纯函数，因此与平台无关。

func rec(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Date(2026, 10, 17, 14, 37, 55, 0, time.Local), level, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

// TestFormatRecordShortStaysOneLine 短记录应当就是一行：时间 + 中文级别 + 正文 + 属性。
func TestFormatRecordShortStaysOneLine(t *testing.T) {
	lines := formatRecord(rec(slog.LevelInfo, "hosts 直连 MITM 已就绪", slog.Int("domains", 6)), nil, "")
	if len(lines) != 1 {
		t.Fatalf("短记录应当只占一行，实际 %d 行：%v", len(lines), lines)
	}
	text := lines[0].plain()
	if text != "14:37:55  信息  hosts 直连 MITM 已就绪  ·  domains=6" {
		t.Errorf("显示排版不符：%q", text)
	}
	if strings.Contains(text, "time=") || strings.Contains(text, "level=") {
		t.Error("窗口里不应出现机器可读的键名（time=/level=）")
	}
	// 级别词用次要色、正文用正文色：颜色只强化层级，不喧宾夺主
	if got := lines[0].Spans[2]; got.Text != "信息" || got.Style != styleMuted {
		t.Errorf("信息级别应当用小号中性色，实际 %+v", got)
	}
}

// TestFormatRecordLongAttrWraps 属性太长时另起续行：正文保持清爽，不糊成一整行。
func TestFormatRecordLongAttrWraps(t *testing.T) {
	err := `写入 hosts 需要管理员权限: open C:\WINDOWS\System32\drivers\etc\hosts: Access is denied.`
	lines := formatRecord(rec(slog.LevelError, "hosts 模式启动失败（系统代理模式不受影响）", slog.String("err", err)), nil, "")
	if len(lines) != 2 {
		t.Fatalf("长属性应换到续行，实际 %d 行", len(lines))
	}
	if got := lines[0].plain(); got != "14:37:55  错误  hosts 模式启动失败（系统代理模式不受影响）" {
		t.Errorf("第一行应只放时间/级别/正文：%q", got)
	}
	if got := lines[1].plain(); !strings.HasPrefix(got, logIndent+"· err="+err[:12]) {
		t.Errorf("续行应缩进并带属性：%q", got)
	}
	// 错误级别：红色 + 加粗（要能一眼扫到）
	if got := lines[0].Spans[2]; got.Style != styleDanger || !got.Bold {
		t.Errorf("错误级别应当用红色加粗，实际 %+v", got)
	}
}

// TestFormatRecordSanitizesNewlines 多行消息在窗口里压成一行，不破坏扫读节奏。
func TestFormatRecordSanitizesNewlines(t *testing.T) {
	lines := formatRecord(rec(slog.LevelWarn, "第一行\n第二行\t带制表"), nil, "")
	if len(lines) != 1 {
		t.Fatalf("换行应被压平，实际 %d 行", len(lines))
	}
	if got := lines[0].plain(); strings.ContainsAny(got, "\n\t") {
		t.Errorf("显示文本里不应残留换行/制表：%q", got)
	}
}

// TestFormatRecordGroupsAndValues 属性值的各种类型与分组都要能显示成人看得懂的样子。
func TestFormatRecordGroupsAndValues(t *testing.T) {
	r := rec(slog.LevelInfo, "混合属性",
		slog.Bool("on", true),
		slog.Duration("耗时", 1500*time.Millisecond),
		slog.Group("proxy", slog.Int("active", 3)),
	)
	lines := formatRecord(r, []slog.Attr{slog.String("阶段", "接管")}, "启动.")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.plain())
		b.WriteByte('\n')
	}
	text := b.String()
	for _, want := range []string{"on=true", "耗时=1.5s", "proxy.active=3", "启动.阶段=接管"} {
		if !strings.Contains(text, want) {
			t.Errorf("属性 %q 没有出现在显示文本里：%q", want, text)
		}
	}
}

// TestLevelWordsAndStyles 级别到「中文词 + 颜色」的映射是配色契约的一部分。
func TestLevelWordsAndStyles(t *testing.T) {
	for _, tc := range []struct {
		level slog.Level
		word  string
		style logStyle
	}{
		{slog.LevelDebug, "调试", styleFaint},
		{slog.LevelInfo, "信息", styleMuted},
		{slog.LevelWarn, "警告", styleWarn},
		{slog.LevelError, "错误", styleDanger},
	} {
		if got := levelWord(tc.level); got != tc.word {
			t.Errorf("%v 的级别词应为 %q，实际 %q", tc.level, tc.word, got)
		}
		if got := levelStyle(tc.level); got != tc.style {
			t.Errorf("%v 的样式应为 %d，实际 %d", tc.level, tc.style, got)
		}
	}
}

// TestRenderSummary 启动摘要：标题 + 每行「标记 + 标签 + 取值」，标记决定颜色。
func TestRenderSummary(t *testing.T) {
	rows := []summaryRow{
		{Mark: markOK, Label: "系统代理", Value: "已接管"},
		{Mark: markWarn, Label: "Hosts", Value: "未生效（需要以管理员身份运行）"},
		{Mark: markInfo, Label: "服务组", Value: "steam、github"},
	}
	lines := renderSummary("通途已启动", rows)
	if len(lines) != 4 {
		t.Fatalf("摘要应为 1 行标题 + 3 行内容，实际 %d", len(lines))
	}
	if got := lines[0]; got.Spans[0].Style != styleAccent || !got.Spans[0].Bold {
		t.Errorf("标题应当用强调色加粗：%+v", got)
	}
	plain := renderSummaryText("通途已启动", rows)
	for _, want := range []string{"通途已启动\n", "  ✓ 系统代理　已接管", "  ！ Hosts　未生效（需要以管理员身份运行）", "  · 服务组　steam、github"} {
		if !strings.Contains(plain, want) {
			t.Errorf("摘要缺少 %q：\n%s", want, plain)
		}
	}
	// 颜色语义：✓=正常绿、！=降级橙且加粗、·=中性
	if s := lines[1].Spans[1]; s.Text != "✓" || s.Style != styleOK {
		t.Errorf("✓ 应为正常色，实际 %+v", s)
	}
	if s := lines[2].Spans[1]; s.Text != "！" || s.Style != styleWarn || !s.Bold {
		t.Errorf("！应为降级色并加粗，实际 %+v", s)
	}
	if s := lines[3].Spans[1]; s.Text != "·" || s.Style != styleMuted {
		t.Errorf("· 应为中性色，实际 %+v", s)
	}
}

// TestWindowHandlerRendersWindowHandler 处理器把记录投喂给显示层，且遵守级别过滤。
func TestWindowHandlerRendersWindowHandler(t *testing.T) {
	sink := &recordingSink{}
	h := newWindowHandler(sink, slog.LevelWarn)
	ctx := context.Background()
	if got := h.Enabled(ctx, slog.LevelInfo); got {
		t.Error("低于设定级别不应输出")
	}
	if got := h.Enabled(ctx, slog.LevelError); !got {
		t.Error("达到级别应输出")
	}
	_ = h.Handle(ctx, rec(slog.LevelError, "出错了"))
	if len(sink.lines) != 1 || !strings.Contains(sink.lines[0].plain(), "错误  出错了") {
		t.Errorf("记录没有渲染进显示层：%+v", sink.lines)
	}
	// WithAttrs 的参数要出现在后续记录里（例如 logger.With("模块", "hosts")）
	h2 := h.WithAttrs([]slog.Attr{slog.String("模块", "hosts")})
	_ = h2.Handle(ctx, rec(slog.LevelError, "又出错了"))
	if got := sink.lines[1].plain(); !strings.Contains(got, "模块=hosts") {
		t.Errorf("WithAttrs 的属性应当出现在记录里：%q", got)
	}
}

// TestLogHandlersFanout 多处理器：一条记录同时进控制台与窗口，级别过滤各自判断。
func TestLogHandlersFanout(t *testing.T) {
	sink := &recordingSink{}
	hs := logHandlers{
		slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelInfo}),
		newWindowHandler(sink, slog.LevelDebug),
	}
	ctx := context.Background()
	if !hs.Enabled(ctx, slog.LevelDebug) {
		t.Error("只要有一个处理器接受该级别，就应当输出")
	}
	if err := hs.Handle(ctx, rec(slog.LevelDebug, "调试信息")); err != nil {
		t.Fatalf("Handle 出错：%v", err)
	}
	if len(sink.lines) != 1 || sink.lines[0].Spans[2].Text != "调试" {
		t.Errorf("窗口这一路应当收到调试记录：%+v", sink.lines)
	}
}

// TestDisplayWidthAndSanitize 显示宽度估算与文本清洗。
func TestDisplayWidthAndSanitize(t *testing.T) {
	if got := displayWidth("服务组 steam"); got != 6+1+5 {
		t.Errorf("中文按 2 列、英文与空格按 1 列：期望 12，实际 %d", got)
	}
	if got := displayWidth(""); got != 0 {
		t.Errorf("空串宽度应为 0，实际 %d", got)
	}
	if got := sanitize("a\nb\r\nc\td"); got != "a b  c d" {
		t.Errorf("换行/制表应压成空格：%q", got)
	}
	if got := sanitize("正常"); got != "正常" {
		t.Errorf("正常文本不应被改动：%q", got)
	}
}

type recordingSink struct{ lines []logLine }

func (s *recordingSink) addLines(lines []logLine) { s.lines = append(s.lines, lines...) }
func (s *recordingSink) addSummary(title string, rows []summaryRow) {
	s.lines = append(s.lines, renderSummary(title, rows)...)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
