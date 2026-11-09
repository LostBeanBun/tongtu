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
	// On 是几何层给出的初始开关态。
	// 其余控件（模式胶囊、只看警告…）由窗口状态决定，这里保持零值。
	On bool
	// Dim 是布局层已经确定的不可用态（诊断面板的动作按钮）：保留按钮、弱化显示，
	// 点了会说明原因，而不是突然消失。
	Dim bool
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

	// PanelOpen / PanelKind 记录这一帧卡片区显示的是哪种面板（日志模式下 PanelOpen=false）。
	// 快照接口据此判断当前视图，不用再靠「某个矩形非空」去猜。
	PanelOpen bool
	PanelKind uiPanelKind

	// —— 自定义站点面板（panel.Open 时有效，此时 Filter / Edit 为零值）——
	SiteInput rectT       // 域名输入框（子控件）
	SiteHint  rectT       // 底部一行说明
	SiteList  rectT       // 列表区域（空状态提示画在这里）
	SiteRows  []uiSiteRow // 每一行的位置

	// —— 域名诊断面板（panel.Kind == uiPanelProbe 时有效）——
	ProbeInput  rectT   // 域名输入框（子控件）
	ProbeRows   []rectT // 固定 5 行的矩形（结论 / 解析 / TCP / 直连 / 经代理）
	ProbeLabelW int32   // 左侧标签列的宽度（右侧值从这里开始）
	ProbeAdvice rectT   // 「下一步做什么」的多行说明
	ProbeHint   rectT   // 底部一行说明
	ProbeHost   string  // 当前诊断的目标域名（快照用；不参与几何）
	// 诊断进度与结论（快照用；不参与几何）：脚本据此判断何时结束、结果如何。
	ProbeBusy    bool
	ProbeVerdict string
	// ProbeActionRow 是底部动作按钮所在的横行（按钮本身在 Regions 里）。
	ProbeActionRow rectT
}

// uiPanelKind 是卡片区当前显示的内容。
type uiPanelKind int

const (
	uiPanelSites uiPanelKind = iota // 自定义站点（零值，兼容既有调用）
	uiPanelProbe                    // 单个域名的端到端诊断
)

// uiPanelState 是卡片区的第二种内容：自定义站点面板或域名诊断面板。
//
// 放进几何计算而不是藏进窗口：面板的行数决定每一行的位置，绘制与命中判定必须
// 用同一份数据（与日志区的几何同理），因此它是 computeUILayout 的入参。
type uiPanelState struct {
	Open  bool
	Kind  uiPanelKind
	Sites []uiSite
	// ProbeHost 只在诊断面板有意义：它不决定几何，但会进快照/缓存键，
	// 让 /ui/layout 能告诉脚本「现在诊断的是哪个域名」。
	ProbeHost string
	// ProbeBusy / ProbeVerdict 同样不决定几何，只进快照：
	// 真机脚本靠它们判断一次诊断何时结束、结论是什么（toast 会消失，日志里也没有）。
	ProbeBusy    bool
	ProbeVerdict string
	// ProbeActions 是这一帧诊断面板底部的动作按钮（复制 / 加入站点 / 打开配置…）。
	// 由窗口按结果算好：几何、绘制与命中必须用同一份，避免按钮显示与行为不一致。
	ProbeActions []uiProbeAction
}

// uiSiteRow 是面板里的一行：域名文字在左，「删除」按钮在右。
type uiSiteRow struct {
	Pattern   string
	Rect      rectT
	TextRight int32 // 域名文字的右边界（再往右是「删除」按钮）
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
		{ID: uiIDQuit, Label: "退出", Kind: uiGhost, Tip: "退出：停止加速并还原系统代理（点窗口的 × 只是隐藏窗口）"},
		{ID: uiIDLog, Label: "日志", Kind: uiGhost, Tip: uiLogTip},
		{ID: uiIDConfig, Label: "打开配置", Kind: uiGhost, Tip: "打开配置文件：用记事本打开 config.yaml"},
		{ID: uiIDServices, Label: "服务组", Kind: uiGhost, Tip: "加速服务组：选择要为哪些站点加速（至少保留一个）"},
		{ID: uiIDSites, Label: "站点", Kind: uiGhost, Tip: uiSitesTip},
		{ID: uiIDSelfTest, Label: "自检", Kind: uiGhost, Tip: uiSelfTestTip},
		{ID: uiIDProbe, Label: "诊断", Kind: uiGhost, Tip: uiProbeTip},
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
	uiIDSys: "系统代理：接管浏览器与系统流量（无需管理员、无需装证书）。点击开关",
}

const uiOnlyWarnTip = "只看警告：只显示警告与错误级别的日志（便于快速定位失败原因）"
const uiFilterTip = "过滤日志：输入关键字（不区分大小写），只显示命中的行"
const uiSelfTestTip = "自检：逐个测试加速域名的解析、直连与经代理访问，确认加速是否真的生效（约 10 秒）"
const uiLogTip = "日志：打开日志文件 / 复制诊断信息 / 清空窗口日志"
const uiTodoTip = "待办：还有没配置好的事情（例如开机自启），点开可一键处理"
const uiPauseTip = "暂停：选一个暂停时长（15 分钟 / 30 分钟 / 1 小时 / 2 小时，或一直停到手动恢复）；进程继续运行，随时可点「恢复」"
const uiResumeTip = "恢复：把暂停前开着的加速项原样打开"
const uiSitesTip = "站点：管理自定义加速域名（例如 huggingface.co）；再点一次回到日志"
const uiSiteAddTip = "添加：把输入框里的域名加入加速白名单（粘贴完整网址也可以）"
const uiSiteDelTip = "删除：从加速白名单里移除这个域名"

// uiSitesEmptyHint 是面板里没有站点时显示的话：告诉用户下一步做什么，而不是一片空白。
const uiSitesEmptyHint = "还没有自定义站点。\n在上面的输入框里填一个域名（例如 huggingface.co），点「添加」即可。"

// uiSitesHint 是面板底部的一行说明：把「立即生效」和写法边界说清楚。
const uiSitesHint = "添加后立即生效；支持 *.example.com 通配，粘贴完整网址也可以。"

// computeUILayout 计算一帧的几何。
//
// 布局是三层栅格：
//
//	第一行：小图标 + 标题 + 状态词（左）      —— 开关胶囊（右）
//	第二行：详情 / 轻量提示（左）             —— 动作按钮（右）
//	卡片  ：工具条（过滤框 + 只看警告）       —— 日志区
//
// 「站点」面板打开时卡片区换成第二种内容：域名输入框 + 站点列表 + 底部说明。
// 两种内容共用同一个卡片矩形与工具条位置，切换时上半部分不会跳动。
//
// 右对齐的控件从右往左排，因此加/减一个按钮不会让左边的内容跳动。
// measure 用正文字体量宽度，measureTitle 用标题字体——两者字号不同，混用会把标题算窄。
func computeUILayout(clientW, clientH int32, dpi uint32, st windowStatus, panel uiPanelState,
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
	l.PanelOpen, l.PanelKind = panel.Open, panel.Kind

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

	// —— 卡片：描边矩形 + 工具条 + 内容（日志 / 站点面板）——
	cardTop := pad + headH
	l.Card = rectT{Left: pad - 1, Top: cardTop - 1, Right: clientW - pad + 1, Bottom: clientH - pad + 1}
	contentL, contentR := pad+inset, clientW-pad-inset
	toolY := cardTop + inset
	toolH := scaleDPI(uiToolbarDIP, dpi)
	ctrlY := toolY + (toolH-ctrlH)/2
	sepY := toolY + toolH + scaleDPI(2, dpi)
	l.FilterSep = rectT{Left: contentL, Top: sepY, Right: contentR, Bottom: sepY + 1}

	if panel.Open {
		if panel.Kind == uiPanelProbe {
			layoutProbePanel(&l, panel, contentL, contentR, ctrlY, sepY, clientH-pad-inset,
				dpi, ctrlH, gap, btnPad, measure)
		} else {
			layoutSitePanel(&l, panel, contentL, contentR, ctrlY, sepY, clientH-pad-inset,
				dpi, ctrlH, gap, chipPad, btnPad, dot, measure)
		}
		return l
	}

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

// layoutSitePanel 计算「自定义站点」面板的几何（computeUILayout 的面板分支）。
//
// 从上到下：输入框 + 添加按钮 / 发丝线 / 站点行（域名 + 删除）/ 底部说明。
// 行数放不下时只画能放下的部分，并让最后一行后面留出说明的位置——面板不做滚动：
// 自定义站点通常只有几条，滚动条会引入一整套额外的交互与几何状态。
func layoutSitePanel(l *uiLayout, panel uiPanelState, contentL, contentR, ctrlY, sepY, bottom int32,
	dpi uint32, ctrlH, gap, chipPad, btnPad, dot int32, measure func(string) int32) {
	addW := 2*btnPad + measure("添加")
	inputW := contentR - contentL - gap - addW
	if inputW < scaleDPI(uiSiteInputMinDIP, dpi) {
		// 窗口很窄时输入框不再保证最小宽度，但「添加」必须留着——它是这个面板的
		// 核心动作；只有挤到输入框都没地方了，才放弃按钮、让输入框占满整行。
		if inputW <= 0 {
			inputW = contentR - contentL
			addW = 0
		}
	}
	l.SiteInput = rectT{Left: contentL, Top: ctrlY, Right: contentL + inputW, Bottom: ctrlY + ctrlH}
	if addW > 0 {
		ax := l.SiteInput.Right + gap
		l.Regions = append(l.Regions, uiRegion{
			ID: uiIDSiteAdd, Label: "添加", Kind: uiGhost, Tip: uiSiteAddTip,
			Rect: rectT{Left: ax, Top: ctrlY, Right: ax + addW, Bottom: ctrlY + ctrlH},
		})
	}

	listTop := sepY + 1 + scaleDPI(6, dpi)
	hintH := scaleDPI(uiSiteHintDIP, dpi)
	l.SiteHint = rectT{Left: contentL, Top: bottom - hintH, Right: contentR, Bottom: bottom}
	listBottom := l.SiteHint.Top - scaleDPI(6, dpi)
	if listBottom < listTop {
		listBottom = listTop
	}
	l.SiteList = rectT{Left: contentL, Top: listTop, Right: contentR, Bottom: listBottom}

	rowH := ctrlH
	y := listTop
	for _, s := range panel.Sites {
		if y+rowH > listBottom {
			break // 放不下的行不画（见上：面板不做滚动）
		}
		row := rectT{Left: contentL, Top: y, Right: contentR, Bottom: y + rowH}
		delW := 2*btnPad + measure("删除")
		delX := contentR - delW
		l.Regions = append(l.Regions, uiRegion{
			ID: uiSiteDelID(s.Pattern), Label: "删除", Kind: uiGhost, Tip: uiSiteDelTip,
			Rect: rectT{Left: delX, Top: y, Right: delX + delW, Bottom: y + rowH},
		})
		textRight := delX - gap
		if textRight < contentL {
			textRight = contentL
		}
		l.SiteRows = append(l.SiteRows, uiSiteRow{Pattern: s.Pattern, Rect: row, TextRight: textRight})
		y += rowH + scaleDPI(4, dpi)
	}
}

// layoutProbePanel 计算「域名诊断」面板的几何（computeUILayout 的面板分支）。
//
// 从上到下：输入框 + 诊断按钮 / 发丝线 / 固定 5 行（标签 + 值）/ 下一步建议 / 底部说明。
// 行数与位置不随结果变化，因此诊断前后面板不会「长高」或「缩短」；窗口不够高时
// 只画放得下的行（与站点面板一样不做滚动），保证建议与底部说明始终可见。
func layoutProbePanel(l *uiLayout, panel uiPanelState, contentL, contentR, ctrlY, sepY, bottom int32,
	dpi uint32, ctrlH, gap, btnPad int32, measure func(string) int32) {
	l.ProbeHost = panel.ProbeHost
	l.ProbeBusy = panel.ProbeBusy
	l.ProbeVerdict = panel.ProbeVerdict

	runW := 2*btnPad + measure("诊断")
	inputW := contentR - contentL - gap - runW
	if inputW <= 0 {
		// 窄到放不下按钮时先保住输入框：没有输入框这个面板就没有意义。
		inputW = contentR - contentL
		runW = 0
	}
	l.ProbeInput = rectT{Left: contentL, Top: ctrlY, Right: contentL + inputW, Bottom: ctrlY + ctrlH}
	if runW > 0 {
		rx := contentR - runW
		l.Regions = append(l.Regions, uiRegion{
			ID: uiIDProbeRun, Label: "诊断", Kind: uiGhost, Tip: uiProbeRunTip,
			Rect: rectT{Left: rx, Top: ctrlY, Right: rx + runW, Bottom: ctrlY + ctrlH},
		})
	}

	// 从下往上摆：底部说明 → 动作行 → 建议区 → 5 行数据。
	// 动作行即使还没有结果也占位（「复制结果」只是弱化），诊断前 / 中 / 后行位置不变。
	hintH := scaleDPI(uiSiteHintDIP, dpi)
	l.ProbeHint = rectT{Left: contentL, Top: bottom - hintH, Right: contentR, Bottom: bottom}
	listTop := sepY + 1 + scaleDPI(8, dpi)
	adviceH := scaleDPI(uiProbeAdviceDIP, dpi)

	adviceBottom := l.ProbeHint.Top - gap
	actionTop, actionBottom := int32(0), int32(0)
	if len(panel.ProbeActions) > 0 {
		actionBottom = l.ProbeHint.Top - gap
		actionTop = actionBottom - ctrlH
		// 窗口太矮时先放弃动作行：建议与数据行比按钮更重要。
		if actionTop-gap-adviceH < listTop {
			actionTop, actionBottom = 0, 0
		} else {
			adviceBottom = actionTop - gap
		}
	}
	adviceTop := adviceBottom - adviceH
	if adviceTop < listTop {
		adviceTop = listTop
	}
	if adviceBottom < adviceTop {
		adviceBottom = adviceTop
	}
	l.ProbeAdvice = rectT{Left: contentL, Top: adviceTop, Right: contentR, Bottom: adviceBottom}
	l.ProbeActionRow = rectT{Left: contentL, Top: actionTop, Right: contentR, Bottom: actionBottom}

	rowsBottom := l.ProbeAdvice.Top - gap
	if rowsBottom < listTop {
		rowsBottom = listTop
	}

	// 标签列按最长标签量一次：值统一从同一列开始，看起来才像一张表。
	labelW := int32(0)
	for _, label := range uiProbeLabels {
		if w := measure(label); w > labelW {
			labelW = w
		}
	}
	labelW += scaleDPI(uiProbeLabelGapDIP, dpi)
	if max := (contentR - contentL) / 2; labelW > max {
		labelW = max
	}
	if labelW < 0 {
		labelW = 0
	}
	l.ProbeLabelW = labelW

	rowH := scaleDPI(uiProbeRowDIP, dpi)
	rowGap := scaleDPI(uiProbeRowGapDIP, dpi)
	y := listTop
	for range uiProbeLabels {
		if y+rowH > rowsBottom {
			break // 放不下的行不画（固定行数是为了不跳动，不是为了强行塞满）
		}
		l.ProbeRows = append(l.ProbeRows, rectT{Left: contentL, Top: y, Right: contentR, Bottom: y + rowH})
		y += rowH + rowGap
	}

	// 动作按钮从左往右排：读起来与建议文字同向。窄窗口放不下时少显示一个，
	// 不压到已有控件；图标 / 文字都在 drawRegion 里统一画。
	if actionBottom > actionTop {
		x := contentL
		for _, a := range panel.ProbeActions {
			w := 2*btnPad + measure(a.Label)
			if x+w > contentR {
				break
			}
			l.Regions = append(l.Regions, uiRegion{
				ID: a.ID, Label: a.Label, Kind: uiGhost, Tip: a.Tip, Dim: a.Dim,
				Rect: rectT{Left: x, Top: actionTop, Right: x + w, Bottom: actionBottom},
			})
			x += w + gap
		}
	}
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

// hasRegion 报告这一帧里是否还有这个 id 的自绘区域（键盘焦点是否仍然有效）。
func (l uiLayout) hasRegion(id string) bool {
	for _, r := range l.Regions {
		if r.ID == id {
			return true
		}
	}
	return false
}

// drawRegion 按区域类型与交互状态绘制一块自绘控件。
//
// 底色只表达交互（悬停 / 按下 / 选中），常态不铺底；状态仍由胶囊里的状态点承担
// （开=绿、关=灰），文字只用文字色阶——沿用「一个颜色只干一件事」的规则，
// 交互底色因此永远压不到状态点下面，对比度不受影响。
//
// dim 表示「当前不可用」（例如站点面板的「添加」按钮在输入框为空时）：只把文字
// 降到弱化色阶，不给悬停底色——看起来不能点的东西不该在鼠标移上去时发亮。
//
// focus 表示这块控件当前持有键盘焦点：在控件内侧画两圈文字色的焦点环。用中性文字色
// 而不是强调色，是因为强调色只负责标题（「一个颜色只干一件事」）；焦点表达的是
// 「键盘现在停在哪」，与状态无关。
func drawRegion(hdc uintptr, theme uiTheme, font uintptr, r uiRegion, dpi uint32, on, hover, press, dim, focus bool) {
	if hdc == 0 || font == 0 || r.Rect.Right <= r.Rect.Left || r.Rect.Bottom <= r.Rect.Top {
		return
	}
	pad := scaleDPI(uiChipPadDIP, dpi)
	radius := (r.Rect.Bottom - r.Rect.Top) / 2 // 胶囊：整高的一半 = 完整半圆
	if r.Kind == uiGhost {
		pad = scaleDPI(uiBtnPadDIP, dpi)
		radius = scaleDPI(uiGhostRadiusDIP, dpi)
	}

	active := !dim && (hover || press)
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
	if dim {
		color = theme.faint // 不可用：三级文字（仍是可读的弱化色，不是隐藏）
	}
	procSetTextColor.Call(hdc, uintptr(color))
	drawTextLine(hdc, r.Label, x, cy, r.Rect.Right-pad-x, dtLeft)

	// 键盘焦点环画在最后：两圈 1 像素，落在控件内侧，不会盖住文字（文字有内边距）。
	if focus && theme.textPen != 0 && theme.nullBrush != 0 &&
		r.Rect.Right-r.Rect.Left > 8 && r.Rect.Bottom-r.Rect.Top > 8 {
		oldPen, _, _ := procSelectObject.Call(hdc, theme.textPen)
		oldBrush, _, _ := procSelectObject.Call(hdc, theme.nullBrush)
		for inset := int32(1); inset <= 2; inset++ {
			procRoundRect.Call(hdc,
				uintptr(r.Rect.Left+inset), uintptr(r.Rect.Top+inset),
				uintptr(r.Rect.Right-inset), uintptr(r.Rect.Bottom-inset),
				uintptr(2*radius+2), uintptr(2*radius+2))
		}
		procSelectObject.Call(hdc, oldBrush)
		procSelectObject.Call(hdc, oldPen)
	}
}

// regionAt 是给窗口用的便捷封装：先确保几何是最新的，再做命中判定。
//
// 为什么要「确保最新」而不是直接读缓存：窗口隐藏时收不到 WM_PAINT，几何缓存可能落后于
// 最新状态（例如状态项变了但还没重画），点上去就会错位。
func (w *logWindow) regionAt(x, y int32) (uiRegion, bool) {
	return w.ensureLayout().hit(x, y)
}
