package main

// 窗口界面的「几何层」：把一帧里所有自绘控件的位置一次算清。
//
// 为什么单独抽出来：绘制与命中判定（鼠标、光标、悬浮提示）必须用同一份几何，
// 否则会出现「看着能点、点不到」。这里只做纯计算（文本度量由 measure 注入），
// 因此可以脱离窗口句柄单测（见 ui_layout_windows_test.go）。

import "strconv"

// uiRegionKind 决定一块区域怎么画。
type uiRegionKind int

const (
	uiChip  uiRegionKind = iota // 胶囊：状态点 + 文字（主开关）
	uiGhost                     // 幽灵按钮：只有文字（次要动作）
)

// uiRegion 是一块可交互区域（自绘控件）。
type uiRegion struct {
	ID    string // 动作 id（见 ui_actions.go）
	Rect  rectT
	Label string
	Kind  uiRegionKind
	Tip   string // 悬浮说明；空表示不给提示
}

// uiLayout 是一帧的完整几何。
type uiLayout struct {
	DPI       uint32
	Icon      rectT
	Title     rectT // 标题（宽度已按可用空间收敛，绘制时会省略号截断）
	State     rectT // 运行状态词
	Detail    rectT // 第二行左侧：详情 / 轻量提示共用这块
	Filter    rectT // 过滤输入框（子控件）
	FilterSep rectT // 工具条下方的发丝线
	Edit      rectT // 日志控件
	Card      rectT // 卡片描边矩形
	Regions   []uiRegion
}

// uiActionButtons 返回第二行的动作按钮：从右到左摆放，顺序即显示顺序。
//
// 「待办」只在确实有需要处理的事情时出现——没问题时界面不该多一个让人猜的按钮。
// 「清空日志」收进了「日志」菜单：它很少用，第二行的宽度要留给常用动作。
func uiActionButtons(st windowStatus) []uiRegion {
	// 暂停 / 恢复是同一个按钮的两种状态：随时一键撤掉全部对外影响，再一键原样回来。
	// 标签只差一个字、同宽，切换时不会让整排按钮左右跳动。
	pauseLabel, pauseTip := "暂停", uiPauseTip
	if st.Paused {
		pauseLabel, pauseTip = "恢复", uiResumeTip
	}
	out := []uiRegion{
		{ID: uiIDQuit, Label: "退出", Kind: uiGhost, Tip: "退出：停止加速并还原系统代理与 hosts（点窗口的 × 只是隐藏窗口）"},
		{ID: uiIDLog, Label: "日志", Kind: uiGhost, Tip: uiLogTip},
		{ID: uiIDConfig, Label: "打开配置", Kind: uiGhost, Tip: "打开配置文件：用记事本打开 config.yaml"},
		{ID: uiIDServices, Label: "服务组", Kind: uiGhost, Tip: "加速服务组：选择要为哪些站点加速（至少保留一个）"},
		{ID: uiIDSelfTest, Label: "自检", Kind: uiGhost, Tip: uiSelfTestTip},
		{ID: uiIDPause, Label: pauseLabel, Kind: uiGhost, Tip: pauseTip},
	}
	if st.TodoN > 0 {
		out = append(out, uiRegion{
			ID: uiIDTodo, Label: "待办 " + strconv.Itoa(st.TodoN), Kind: uiGhost, Tip: uiTodoTip,
		})
	}
	return out
}

// 胶囊的悬浮说明：与托盘菜单里的文案保持一致，用户在哪边看都是同一句话。
var uiModeTips = map[string]string{
	uiIDSys:   "系统代理模式：接管浏览器与系统流量（无需管理员）。点击开关",
	uiIDHosts: "Hosts 模式：覆盖 hosts 解析，供 Steam 客户端等不走代理的程序使用（需管理员）。点击开关",
	uiIDMITM:  "无 SNI 反代：本地解密被 SNI 封锁的域名，出站剥离 SNI（需先安装根证书）。点击开关",
}

const uiOnlyWarnTip = "只看警告：只显示警告与错误级别的日志（便于快速定位失败原因）"
const uiFilterTip = "过滤日志：输入关键字（不区分大小写），只显示命中的行"
const uiSelfTestTip = "自检：逐个测试加速域名的解析、直连与经代理访问，确认加速是否真的生效（约 10 秒）"
const uiLogTip = "日志：打开日志文件 / 复制诊断信息 / 清空窗口日志"
const uiTodoTip = "待办：还有没配置好的事情（安装证书、以管理员重启、开机自启…），点开可一键处理"
const uiPauseTip = "暂停：选一个暂停时长（15 分钟 / 30 分钟 / 1 小时 / 2 小时，或一直停到手动恢复）；进程继续运行，随时可点「恢复」"
const uiResumeTip = "恢复：把暂停前开着的加速项原样打开"

// computeUILayout 计算一帧的几何。
//
// 布局是三层栅格：
//
//	第一行：小图标 + 标题 + 状态词（左）      —— 开关胶囊（右）
//	第二行：详情 / 轻量提示（左）             —— 动作按钮（右）
//	卡片  ：工具条（过滤框 + 只看警告）       —— 日志区
//
// 右对齐的控件从右往左排，因此加/减一个按钮不会让左边的内容跳动。
// measure 用正文字体量宽度，measureTitle 用标题字体——两者字号不同，混用会把标题算窄。
func computeUILayout(clientW, clientH int32, dpi uint32, st windowStatus,
	measure func(string) int32, measureTitle func(string) int32) uiLayout {
	pad := scaleDPI(uiPadDIP, dpi)
	row1 := scaleDPI(uiRow1DIP, dpi)
	row2 := scaleDPI(uiRow2DIP, dpi)
	headH := row1 + row2
	ctrlH := scaleDPI(uiControlDIP, dpi)
	gap := scaleDPI(uiGapDIP, dpi)
	chipPad := scaleDPI(uiChipPadDIP, dpi)
	btnPad := scaleDPI(uiBtnPadDIP, dpi)
	dot := scaleDPI(uiDotDIP, dpi)
	iconSide := scaleDPI(uiIconDIP, dpi)
	inset := scaleDPI(uiCardPadDIP, dpi)

	var l uiLayout
	l.DPI = dpi

	// —— 第一行：图标 / 标题 / 状态词 + 开关胶囊 ——
	l.Icon = rectT{Left: pad, Top: (row1 - iconSide) / 2, Right: pad + iconSide, Bottom: (row1 + iconSide) / 2}
	titleX := l.Icon.Right + scaleDPI(uiIconGapDIP, dpi)

	items := modeItems(st.Items)
	x := clientW - pad
	for i := len(items) - 1; i >= 0; i-- {
		w := 2*chipPad + 2*dot + scaleDPI(6, dpi) + measure(items[i].Label)
		x -= w
		l.Regions = append(l.Regions, uiRegion{
			ID:    items[i].Action,
			Label: items[i].Label,
			Kind:  uiChip,
			Tip:   pickTip(items[i].Tip, uiModeTips[items[i].Action]),
			Rect:  rectT{Left: x, Top: (row1 - ctrlH) / 2, Right: x + w, Bottom: (row1 + ctrlH) / 2},
		})
		x -= gap
	}
	capStart := x + gap

	// 标题与状态词：宽度不够时先压缩标题（状态词是健康度，比品牌名更需要完整显示）
	avail := capStart - gap - titleX
	if avail < 0 {
		avail = 0
	}
	tw, sw, sgap := measureTitle(st.Title), int32(0), int32(0)
	if st.State != "" {
		sgap = scaleDPI(uiTitleGapDIP, dpi)
		sw = measure(st.State)
	}
	if need := tw + sgap + sw; need > avail {
		tw = avail - sgap - sw
		if tw < 0 {
			tw = 0
		}
	}
	l.Title = rectT{Left: titleX, Right: titleX + tw, Bottom: row1}
	l.State = rectT{Left: l.Title.Right + sgap, Right: l.Title.Right + sgap + sw, Bottom: row1}

	// —— 第二行：动作按钮（右）+ 详情（左）——
	y2 := row1
	bx := clientW - pad
	for _, b := range uiActionButtons(st) {
		w := 2*btnPad + measure(b.Label)
		bx -= w
		r := rectT{Left: bx, Top: y2 + (row2-ctrlH)/2, Right: bx + w, Bottom: y2 + (row2-ctrlH)/2 + ctrlH}
		l.Regions = append(l.Regions, uiRegion{ID: b.ID, Label: b.Label, Kind: b.Kind, Tip: b.Tip, Rect: r})
		bx -= gap
	}
	l.Detail = rectT{Left: pad, Top: y2, Right: bx, Bottom: y2 + row2}

	// —— 卡片：描边矩形 + 工具条 + 日志区 ——
	cardTop := pad + headH
	l.Card = rectT{Left: pad - 1, Top: cardTop - 1, Right: clientW - pad + 1, Bottom: clientH - pad + 1}
	contentL, contentR := pad+inset, clientW-pad-inset
	toolY := cardTop + inset
	toolH := scaleDPI(uiToolbarDIP, dpi)
	ctrlY := toolY + (toolH-ctrlH)/2

	filterW := scaleDPI(uiFilterDIP, dpi)
	onlyW := 2*btnPad + measure("只看警告")
	if maxW := contentR - contentL - gap - onlyW; filterW > maxW {
		filterW = maxW
	}
	if filterW >= scaleDPI(uiFilterMinDIP, dpi) {
		l.Filter = rectT{Left: contentL, Top: ctrlY, Right: contentL + filterW, Bottom: ctrlY + ctrlH}
		if ox := l.Filter.Right + gap; ox+onlyW <= contentR {
			l.Regions = append(l.Regions, uiRegion{
				ID: uiIDOnlyWarn, Label: "只看警告", Kind: uiGhost, Tip: uiOnlyWarnTip,
				Rect: rectT{Left: ox, Top: ctrlY, Right: ox + onlyW, Bottom: ctrlY + ctrlH},
			})
		}
	}
	sepY := toolY + toolH + scaleDPI(2, dpi)
	l.FilterSep = rectT{Left: contentL, Top: sepY, Right: contentR, Bottom: sepY + 1}
	l.Edit = rectT{
		Left:   contentL,
		Top:    sepY + 1 + scaleDPI(6, dpi),
		Right:  contentR,
		Bottom: clientH - pad - inset,
	}
	if l.Edit.Bottom < l.Edit.Top {
		l.Edit.Bottom = l.Edit.Top
	}
	return l
}

// pickTip 优先用状态里带来的说明（它知道「需不需要管理员」这类上下文），
// 没带就用界面里的默认文案。
func pickTip(fromStatus, fallback string) string {
	if fromStatus != "" {
		return fromStatus
	}
	return fallback
}

// hit 返回坐标命中的自绘区域。
func (l uiLayout) hit(x, y int32) (uiRegion, bool) {
	for _, r := range l.Regions {
		if x >= r.Rect.Left && x < r.Rect.Right && y >= r.Rect.Top && y < r.Rect.Bottom {
			return r, true
		}
	}
	return uiRegion{}, false
}

// drawRegion 按区域类型与交互状态绘制一块自绘控件。
//
// 底色只表达交互（悬停 / 按下 / 选中），常态不铺底；状态仍由胶囊里的状态点承担
// （开=绿、关=灰），文字只用文字色阶——沿用「一个颜色只干一件事」的规则，
// 交互底色因此永远压不到状态点下面，对比度不受影响。
func drawRegion(hdc uintptr, theme uiTheme, font uintptr, r uiRegion, dpi uint32, on, hover, press bool) {
	if hdc == 0 || font == 0 || r.Rect.Right <= r.Rect.Left || r.Rect.Bottom <= r.Rect.Top {
		return
	}
	pad := scaleDPI(uiChipPadDIP, dpi)
	radius := (r.Rect.Bottom - r.Rect.Top) / 2 // 胶囊：整高的一半 = 完整半圆
	if r.Kind == uiGhost {
		pad = scaleDPI(uiBtnPadDIP, dpi)
		radius = scaleDPI(uiGhostRadiusDIP, dpi)
	}

	active := hover || press
	// 只在这一刻铺底：鼠标悬停/按下，或（幽灵按钮）当前处于选中态
	if active || (r.Kind == uiGhost && on) {
		oldBrush, _, _ := procSelectObject.Call(hdc, theme.hoverBrush)
		oldPen, _, _ := procSelectObject.Call(hdc, theme.nullPen)
		procRoundRect.Call(hdc, uintptr(r.Rect.Left), uintptr(r.Rect.Top),
			uintptr(r.Rect.Right), uintptr(r.Rect.Bottom),
			uintptr(2*radius+2), uintptr(2*radius+2))
		procSelectObject.Call(hdc, oldPen)
		procSelectObject.Call(hdc, oldBrush)
	}

	cy := (r.Rect.Top + r.Rect.Bottom) / 2
	x := r.Rect.Left + pad
	if r.Kind == uiChip {
		dot := scaleDPI(uiDotDIP, dpi)
		brush := theme.offBrush
		if on {
			brush = theme.okBrush
		}
		procSelectObject.Call(hdc, brush)
		procEllipse.Call(hdc, uintptr(x), uintptr(cy-dot), uintptr(x+2*dot), uintptr(cy+dot))
		x += 2*dot + scaleDPI(6, dpi)
	}

	procSelectObject.Call(hdc, font)
	color := theme.muted // 关闭 / 常态：二级文字（落在窗口底上仍有 ≥4.5:1）
	if on || active {
		color = theme.text
	}
	procSetTextColor.Call(hdc, uintptr(color))
	drawTextLine(hdc, r.Label, x, cy, r.Rect.Right-pad-x, dtLeft)
}

// regionAt 是给窗口用的便捷封装：先确保几何是最新的，再做命中判定。
//
// 为什么要「确保最新」而不是直接读缓存：窗口隐藏时收不到 WM_PAINT，几何缓存可能落后于
// 最新状态（例如状态项变了但还没重画），点上去就会错位。
func (w *logWindow) regionAt(x, y int32) (uiRegion, bool) {
	return w.ensureLayout().hit(x, y)
}
