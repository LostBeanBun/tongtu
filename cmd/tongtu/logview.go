package main

// 日志窗口的「显示层」：把结构化日志渲染成给人看的行（带样式片段）。
//
// 为什么不直接把 slog 的 TextHandler 输出塞进窗口：那是给日志文件与终端看的机器可读格式
// （RFC3339 带纳秒、level=INFO、k=v 与引号转义），在窗口里又长又难扫。窗口只服务于
// 「一眼看清发生了什么」，因此单独渲染一遍：
//   - 时间只留时分秒，级别用两个汉字（宽度一致，天然对齐）；
//   - 级别配色（信息=次要灰、警告=橙、错误=红），颜色只强化，不单独承担信息；
//   - 属性跟在正文后面，太长了就缩进换行，避免糊成一整行。
//
// 日志文件与控制台仍是结构化文本（排查问题需要原始格式），两边的取舍互不影响。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// logStyle 是显示样式；具体颜色由当前主题决定（见 uiTheme.styleColor）。
type logStyle int

const (
	styleText   logStyle = iota // 正文
	styleMuted                  // 次要：时间戳、属性
	styleFaint                  // 更弱：调试级别
	styleAccent                 // 强调：启动摘要标题
	styleOK                     // 成功：✓
	styleWarn                   // 降级/警告：！、警告级别
	styleDanger                 // 错误：错误级别
)

// logSpan 是一段同一风格的文本；logLine 是一行显示内容。
type logSpan struct {
	Style logStyle
	Text  string
	Bold  bool
}

type logLine struct {
	Spans []logSpan
}

// plain 返回不含样式的纯文本（控制台输出、长度统计用）。
func (l logLine) plain() string {
	var b strings.Builder
	for _, s := range l.Spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// size 返回这一行占用的字节数（含换行），用于限制窗口内容总长度。
func (l logLine) size() int {
	n := 2 // CRLF
	for _, s := range l.Spans {
		n += len(s.Text)
	}
	return n
}

// logSink 是日志窗口的显示层接口：Windows 上由 *logWindow 实现，
// 其他平台没有界面，logDisplay 返回 nil（处理器直接跳过）。
type logSink interface {
	addLines([]logLine)
	addSummary(title string, rows []summaryRow)
}

const (
	logTimeLayout = "15:04:05"         // 窗口里的时间只留时分秒
	logIndent     = "                " // 续行缩进（对齐到正文列，仅作视觉分组）
	logLineBudget = 78                 // 单行显示宽度上限（半角列），超出就把属性挪到续行
	logAttrSep    = "  ·  "            // 同一行内多个属性之间的分隔
)

// levelWord 把 slog 级别映射成中文词：都用两个汉字，宽度一致才好对齐。
func levelWord(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "调试"
	case l < slog.LevelWarn:
		return "信息"
	case l < slog.LevelError:
		return "警告"
	default:
		return "错误"
	}
}

// levelStyle 决定级别词的颜色：日常信息用次要灰（不喧宾夺主），警告用降级橙，错误用红。
func levelStyle(l slog.Level) logStyle {
	switch {
	case l < slog.LevelInfo:
		return styleFaint
	case l < slog.LevelWarn:
		return styleMuted
	case l < slog.LevelError:
		return styleWarn
	default:
		return styleDanger
	}
}

// formatRecord 把一条日志记录渲染成显示行。
func formatRecord(r slog.Record, extra []slog.Attr, prefix string) []logLine {
	attrs := attrStrings(extra, prefix)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, attrString(a, prefix)...)
		return true
	})

	// 级别词加粗只在「需要被看见」的级别上（警告/错误），信息级别保持安静。
	bold := r.Level >= slog.LevelWarn
	head := []logSpan{
		{Style: styleMuted, Text: r.Time.Format(logTimeLayout)},
		{Style: styleMuted, Text: "  "},
		{Style: levelStyle(r.Level), Text: levelWord(r.Level), Bold: bold},
		{Style: styleMuted, Text: "  "},
		{Style: styleText, Text: sanitize(r.Message)},
	}
	if len(attrs) == 0 {
		return []logLine{{Spans: head}}
	}

	joined := strings.Join(attrs, logAttrSep)
	used := len(logIndent) + displayWidth(r.Message) + displayWidth(joined) + len(logAttrSep)
	if used <= logLineBudget {
		head = append(head, logSpan{Style: styleMuted, Text: logAttrSep + joined})
		return []logLine{{Spans: head}}
	}
	// 一行放不下：属性各占一行并缩进，正文保持清爽
	lines := []logLine{{Spans: head}}
	for _, a := range attrs {
		lines = append(lines, logLine{Spans: []logSpan{{Style: styleMuted, Text: logIndent + "· " + a}}})
	}
	return lines
}

// summaryMark 是启动摘要每行的标记。
type summaryMark int

const (
	markInfo summaryMark = iota // ·  说明性信息
	markOK                      // ✓  已生效
	markWarn                    // ！ 未生效（降级）
)

// summaryRow 是启动摘要里的一行「标签 + 取值」。
type summaryRow struct {
	Mark  summaryMark
	Label string
	Value string
}

// renderSummary 把启动摘要渲染成显示行：标题一行，之后每行「标记 + 标签 + 取值」。
// 标签与取值之间用全角空格分隔——不用等宽字体也能拉开距离，且不需要对齐计算。
func renderSummary(title string, rows []summaryRow) []logLine {
	if title == "" && len(rows) == 0 {
		return nil
	}
	var out []logLine
	if title != "" {
		out = append(out, logLine{Spans: []logSpan{{Style: styleAccent, Text: title, Bold: true}}})
	}
	for _, r := range rows {
		mark, markStyle, valStyle := "·", styleMuted, styleMuted
		switch r.Mark {
		case markOK:
			mark, markStyle, valStyle = "✓", styleOK, styleText
		case markWarn:
			mark, markStyle, valStyle = "！", styleWarn, styleWarn
		}
		out = append(out, logLine{Spans: []logSpan{
			{Style: styleMuted, Text: "  "},
			{Style: markStyle, Text: mark, Bold: r.Mark == markWarn},
			{Style: styleMuted, Text: " "},
			{Style: styleText, Text: r.Label},
			{Style: styleMuted, Text: "　"},
			{Style: valStyle, Text: r.Value},
		}})
	}
	return out
}

// renderSummaryText 把启动摘要渲染成纯文本（控制台用；窗口另走 renderSummary 上色）。
func renderSummaryText(title string, rows []summaryRow) string {
	lines := renderSummary(title, rows)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.plain())
		b.WriteByte('\n')
	}
	return b.String()
}

// —— 日志过滤（窗口里输入关键字 / 只看警告时用）——

// uiNoMatchHint 是过滤后没有命中时显示的那一行提示。
const uiNoMatchHint = "（没有匹配的日志行 · 清空过滤框可恢复）"

// filterLines 按关键字与「只看警告」过滤显示行。
//
// 规则：
//   - query 是关键字（不区分大小写），按整行纯文本匹配；
//   - onlyWarn 只保留含警告/错误级别的行；
//   - 续行（缩进的属性行，见 logIndent）跟随上一行的取舍——否则一条记录的属性会被拦腰截断。
func filterLines(lines []logLine, query string, onlyWarn bool) []logLine {
	if query == "" && !onlyWarn {
		return lines
	}
	needle := strings.ToLower(query)
	out := make([]logLine, 0, len(lines))
	prevKept := false
	for _, l := range lines {
		text := l.plain()
		keep := false
		if strings.HasPrefix(text, logIndent) {
			keep = prevKept // 续行：跟着上一行走
		} else {
			keep = (!onlyWarn || hasWarnSpan(l)) &&
				(needle == "" || strings.Contains(strings.ToLower(text), needle))
		}
		if keep {
			out = append(out, l)
		}
		prevKept = keep
	}
	return out
}

// hasWarnSpan 判断一行里是否有警告/错误级别的片段（启动摘要里的 ！行也算）。
func hasWarnSpan(l logLine) bool {
	for _, s := range l.Spans {
		if s.Style == styleWarn || s.Style == styleDanger {
			return true
		}
	}
	return false
}

// —— 日志处理器 ——

// logHandlers 让同一条记录同时进入多个处理器：控制台/日志文件走结构化文本，
// 窗口走显示层（见 newLogger）。标准库没有现成的「多路处理器」，这里补一个。
type logHandlers []slog.Handler

func (hs logHandlers) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (hs logHandlers) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range hs {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (hs logHandlers) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(logHandlers, len(hs))
	for i, h := range hs {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (hs logHandlers) WithGroup(name string) slog.Handler {
	if name == "" {
		return hs
	}
	out := make(logHandlers, len(hs))
	for i, h := range hs {
		out[i] = h.WithGroup(name)
	}
	return out
}

// windowHandler 把日志记录渲染进日志窗口（见 formatRecord）。
type windowHandler struct {
	sink   logSink
	level  slog.Level
	attrs  []slog.Attr
	prefix string
}

func newWindowHandler(sink logSink, level slog.Level) *windowHandler {
	return &windowHandler{sink: sink, level: level}
}

func (h *windowHandler) Enabled(_ context.Context, l slog.Level) bool {
	return h.sink != nil && l >= h.level
}

func (h *windowHandler) Handle(_ context.Context, r slog.Record) error {
	if h.sink == nil {
		return nil
	}
	h.sink.addLines(formatRecord(r, h.attrs, h.prefix))
	return nil
}

func (h *windowHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), withPrefix(as, h.prefix)...)
	return &c
}

func (h *windowHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix += name + "."
	return &c
}

// withPrefix 把当前分组前缀烘焙进属性键（显示层只关心「键=值」，不还原分组结构）。
func withPrefix(as []slog.Attr, prefix string) []slog.Attr {
	if prefix == "" {
		return as
	}
	out := make([]slog.Attr, 0, len(as))
	for _, a := range as {
		a.Key = prefix + a.Key
		out = append(out, a)
	}
	return out
}

// attrStrings 把一组属性拍平成 "键=值" 文本。
func attrStrings(as []slog.Attr, prefix string) []string {
	var out []string
	for _, a := range as {
		out = append(out, attrString(a, prefix)...)
	}
	return out
}

// attrString 把单个属性渲染成 "键=值"；分组属性展开为 "组.键"。
func attrString(a slog.Attr, prefix string) []string {
	a.Value = a.Value.Resolve()
	key := prefix + a.Key
	if key == "" {
		return nil
	}
	if a.Value.Kind() == slog.KindGroup {
		var out []string
		for _, ga := range a.Value.Group() {
			out = append(out, attrString(ga, key+".")...)
		}
		if key == "" {
			return out
		}
		return out
	}
	return []string{key + "=" + sanitize(valueString(a.Value))}
}

// valueString 把属性值渲染成人类可读文本（错误类型取 Error()，时间用 RFC3339）。
func valueString(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(v.Any())
	default:
		return v.String()
	}
}

// sanitize 把换行/制表与控制字符压成空格：窗口里一条记录就是一行，
// 多行会破坏「时间 + 级别 + 正文」的扫读节奏（日志文件里仍是原样）。
func sanitize(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || (unicode.IsControl(r) && r != '\x1b')
	}) {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// displayWidth 估算文本在界面上占的半角列数（中日韩全角字符算 2 列）。
// 仅用于决定「属性放同一行还是换行缩进」，不参与精确排版，按区间近似即可。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF, // 中日韩部首、假名、汉字
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // 中日韩兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}
