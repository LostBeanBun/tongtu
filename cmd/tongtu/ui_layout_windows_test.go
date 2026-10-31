//go:build windows

package main

import (
	"testing"
)

// 几何层是纯函数（文本度量由 measure 注入），因此可以脱离窗口单测：
// 布局关系、命中判定、窄窗口下的取舍都在这里锁住。

// 固定字距的假度量：每个字符 10 像素，方便手算期望值。
func fixedMeasure(s string) int32 { return int32(len([]rune(s))) * 10 }

func layoutStatus() windowStatus {
	return windowStatus{
		Title: "通途 Tongtu 0.1.0",
		State: "运行中",
		Level: stateOK,
		Items: []windowStatusItem{
			{Label: "系统代理", On: true, Action: uiIDSys},
			{Label: "Hosts", On: false, Action: uiIDHosts},
			{Label: "无SNI反代", On: true, Action: uiIDMITM},
		},
		Detail: []string{"服务组 Steam", "活跃隧道 2", "↓1.2 MB ↑340 KB"},
	}
}

func TestUILayoutRightAlignsControls(t *testing.T) {
	const w, h = 800, 500
	l := computeUILayout(w, h, 96, layoutStatus(), uiPanelState{}, fixedMeasure, fixedMeasure)
	pad := int32(uiPadDIP)

	// 开关胶囊靠右排：最右一项的右边贴在留白上
	chips := make([]uiRegion, 0, 3)
	for _, r := range l.Regions {
		if r.Kind == uiChip {
			chips = append(chips, r)
		}
	}
	if len(chips) != 3 {
		t.Fatalf("应有三项开关胶囊，实际 %d 项：%+v", len(chips), l.Regions)
	}
	rightmost := chips[0] // Regions 是按从右往左的顺序追加的
	if rightmost.ID != uiIDMITM {
		t.Errorf("最右侧应是最后一项（无SNI反代），实际 %q", rightmost.ID)
	}
	if rightmost.Rect.Right != w-pad {
		t.Errorf("最右胶囊应贴右留白：right=%d 期望 %d", rightmost.Rect.Right, w-pad)
	}
	// 相邻胶囊不重叠，且逐个向左
	for i := 1; i < len(chips); i++ {
		if chips[i].Rect.Right > chips[i-1].Rect.Left {
			t.Errorf("胶囊重叠：%q %+v 与 %q %+v", chips[i].ID, chips[i].Rect, chips[i-1].ID, chips[i-1].Rect)
		}
	}
	// 胶囊都在第一行内（垂直方向不越界）
	for _, c := range chips {
		if c.Rect.Top < 0 || c.Rect.Bottom > int32(uiRow1DIP) {
			t.Errorf("胶囊超出第一行：%+v", c.Rect)
		}
	}

	// 动作按钮靠右排、在第二行
	btns := make([]uiRegion, 0, 6)
	for _, r := range l.Regions {
		if r.Kind == uiGhost {
			btns = append(btns, r)
		}
	}
	// 退出 / 日志 / 打开配置 / 服务组 / 站点 / 自检 / 暂停 + 只看警告（无待办时不出现「待办」）
	if len(btns) != 8 {
		t.Fatalf("应有 8 个幽灵按钮，实际 %d", len(btns))
	}
	if btns[0].ID != uiIDQuit || btns[0].Rect.Right != w-pad {
		t.Errorf("最右侧应是「退出」并贴右留白：%+v", btns[0])
	}
	if btns[0].Rect.Top < int32(uiRow1DIP) {
		t.Errorf("动作按钮应在第二行：%+v", btns[0].Rect)
	}
	// 详情区不能压到按钮上
	firstBtn := btns[6] // 暂停（动作按钮表里的最后一个）
	if firstBtn.ID != uiIDPause {
		t.Fatalf("最左侧的动作按钮应是「暂停」：%+v", firstBtn)
	}
	if l.Detail.Right > firstBtn.Rect.Left {
		t.Errorf("详情区压到了按钮：detail.Right=%d 按钮 left=%d", l.Detail.Right, firstBtn.Rect.Left)
	}
	if l.Detail.Left != pad {
		t.Errorf("详情区应从左侧留白开始，实际 %d", l.Detail.Left)
	}

	// 卡片与日志区的关系：日志区在卡片内边距里、工具条下方
	inset := int32(uiCardPadDIP)
	if l.Edit.Left != l.Card.Left+1+inset || l.Edit.Right != l.Card.Right-1-inset {
		t.Errorf("日志区左右应内缩到卡片内边距：edit=%+v card=%+v inset=%d", l.Edit, l.Card, inset)
	}
	if l.Edit.Bottom != h-pad-inset {
		t.Errorf("日志区底边应贴卡片下内边距：bottom=%d 期望 %d", l.Edit.Bottom, h-pad-inset)
	}
	if l.Edit.Top <= l.FilterSep.Top || l.FilterSep.Top <= l.Filter.Bottom {
		t.Errorf("工具条 / 分隔线 / 日志区的上下关系不对：filter=%+v sep=%+v edit=%+v",
			l.Filter, l.FilterSep, l.Edit)
	}

	// 过滤框宽度：窗口够宽时就是设计值；高度等于控件高
	if got := l.Filter.Right - l.Filter.Left; got != int32(uiFilterDIP) {
		t.Errorf("过滤框宽度应为 %d，实际 %d", uiFilterDIP, got)
	}
	if got := l.Filter.Bottom - l.Filter.Top; got != int32(uiControlDIP) {
		t.Errorf("过滤框高度应为 %d，实际 %d", uiControlDIP, got)
	}
}

func TestUILayoutHitMatchesRegions(t *testing.T) {
	l := computeUILayout(800, 500, 96, layoutStatus(), uiPanelState{}, fixedMeasure, fixedMeasure)
	for _, r := range l.Regions {
		cx := (r.Rect.Left + r.Rect.Right) / 2
		cy := (r.Rect.Top + r.Rect.Bottom) / 2
		got, ok := l.hit(cx, cy)
		if !ok || got.ID != r.ID {
			t.Errorf("区域 %q 的中心点应命中自己，实际 %v/%q", r.ID, ok, got.ID)
		}
	}
	// 卡片中间是空白：不该命中任何区域
	if r, ok := l.hit(400, 400); ok {
		t.Errorf("空白处不该命中区域，实际 %+v", r)
	}
	// 边界：右边界是开的（右开区间），因此 right 本身不算命中
	for _, r := range l.Regions {
		if _, ok := l.hit(r.Rect.Right, (r.Rect.Top+r.Rect.Bottom)/2); ok {
			// 只有当这个点不落在相邻区域里才算「不命中」
			if rr, ok2 := l.hit(r.Rect.Right, (r.Rect.Top+r.Rect.Bottom)/2); ok2 && rr.ID == r.ID {
				t.Errorf("区域 %q 的右边界不该算命中自己", r.ID)
			}
		}
	}
}

func TestUILayoutNarrowWindowDropsFilter(t *testing.T) {
	// 窗口太窄时先放弃过滤框（它只是辅助功能），日志区必须留下
	l := computeUILayout(180, 300, 96, layoutStatus(), uiPanelState{}, fixedMeasure, fixedMeasure)
	if l.Filter.Right > l.Filter.Left {
		t.Errorf("窄窗口下应放弃过滤框，实际 %+v", l.Filter)
	}
	for _, r := range l.Regions {
		if r.ID == uiIDOnlyWarn {
			t.Errorf("没有过滤框时「只看警告」也不该出现：%+v", r)
		}
	}
	if l.Edit.Bottom <= l.Edit.Top || l.Edit.Right <= l.Edit.Left {
		t.Errorf("窄窗口下日志区仍要有空间：%+v", l.Edit)
	}
}

func TestUILayoutTitleGivesWayToStateWord(t *testing.T) {
	// 第一行挤不下时先压缩标题（状态词是健康度，比品牌名重要）
	st := layoutStatus()
	l := computeUILayout(430, 300, 96, st, uiPanelState{}, fixedMeasure, fixedMeasure)
	titleW := l.Title.Right - l.Title.Left
	fullW := fixedMeasure(st.Title)
	if titleW >= fullW {
		t.Errorf("窄窗口下标题应被压缩：%d vs 完整 %d", titleW, fullW)
	}
	if got := l.State.Right - l.State.Left; got != fixedMeasure(st.State) {
		t.Errorf("状态词应完整保留，实际 %d 期望 %d", got, fixedMeasure(st.State))
	}
	if l.State.Right > l.Icon.Right+1000 { // 别跑到右侧胶囊里去
		t.Errorf("状态词位置异常：%+v", l.State)
	}
}

func TestUILayoutWithoutItemsUsesDefaultModes(t *testing.T) {
	// 状态项还没填（窗口刚建好）时不能算出负数或崩溃；
	// 而且此时仍要摆出三个默认开关胶囊——否则窗口起来的第一帧会「少一排按钮」，随后突然跳出来。
	l := computeUILayout(700, 400, 96, windowStatus{}, uiPanelState{}, fixedMeasure, fixedMeasure)
	if l.Edit.Right <= l.Edit.Left {
		t.Errorf("没有状态项时日志区仍应可用：%+v", l.Edit)
	}
	got := map[string]bool{}
	for _, r := range l.Regions {
		if r.Kind == uiChip {
			got[r.ID] = true
			if r.Label == "" {
				t.Errorf("默认胶囊要有标签：%+v", r)
			}
		}
	}
	for _, id := range []string{uiIDSys, uiIDHosts, uiIDMITM} {
		if !got[id] {
			t.Errorf("默认状态项应包含 %q，实际 %v", id, got)
		}
	}
}

func TestResolveThemeBlend(t *testing.T) {
	// blend 的端点语义：alpha=0 保持底色，alpha=1 全换成前景
	base, fg := rgb(0x20, 0x20, 0x20), rgb(0xF0, 0xF0, 0xF0)
	if got := blend(base, fg, 0); got != base {
		t.Errorf("alpha=0 应保持底色，实际 %#x", got)
	}
	if got := blend(base, fg, 1); got != fg {
		t.Errorf("alpha=1 应等于前景，实际 %#x", got)
	}
	mid := blend(base, fg, 0.5)
	if mid == base || mid == fg {
		t.Errorf("中间值应介于两者之间，实际 %#x", mid)
	}

	// 两套主题都要推导出「看得出是不同层」的交互底色
	for _, th := range []uiTheme{themeDark, themeLight} {
		got := resolveTheme(th)
		if got.hover == got.bg || got.input == got.cardBG {
			t.Errorf("主题 %v：交互底色没被推导出来", th.dark)
		}
		if got.hoverBrush == 0 || got.inputBrush == 0 {
			continue // 画刷由 buildBrushes 准备（没有窗口时不建）
		}
	}
}

// TestPauseButtonLabel 暂停 / 恢复共用同一个按钮：文案与提示随状态切换，宽度不变（整排不跳）。
func TestPauseButtonLabel(t *testing.T) {
	for _, c := range []struct {
		paused bool
		label  string
		tip    string
	}{
		{false, "暂停", uiPauseTip},
		{true, "恢复", uiResumeTip},
	} {
		st := layoutStatus()
		st.Paused = c.paused
		var got uiRegion
		found := false
		for _, r := range uiActionButtons(st) {
			if r.ID == uiIDPause {
				got, found = r, true
			}
		}
		if !found {
			t.Fatalf("动作按钮里应始终有「暂停 / 恢复」")
		}
		if got.Label != c.label || got.Tip != c.tip {
			t.Errorf("paused=%v：label=%q tip=%q，期望 %q / %q",
				c.paused, got.Label, got.Tip, c.label, c.tip)
		}
	}

	// 两个标签同宽：切换时按钮位置不应左右跳（用固定字宽的量尺验证）
	off := uiActionButtons(layoutStatus())
	on := layoutStatus()
	on.Paused = true
	onRegions := uiActionButtons(on)
	if len(off) != len(onRegions) {
		t.Fatalf("按钮数量不该变化: %d vs %d", len(off), len(onRegions))
	}
	for i := range off {
		if off[i].ID != onRegions[i].ID {
			t.Fatalf("按钮顺序不该变化: %q vs %q", off[i].ID, onRegions[i].ID)
		}
	}
}

// —— 自定义站点面板 ——

// TestUILayoutSitePanel 面板打开时：输入框 + 添加按钮在工具条位置，下面是站点行，
// 底部是说明；日志区与过滤框让位（不参与绘制 / 命中）。
func TestUILayoutSitePanel(t *testing.T) {
	panel := uiPanelState{Open: true, Sites: []uiSite{
		{Pattern: "huggingface.co", MITM: true},
		{Pattern: "*.githubusercontent.com", MITM: false},
	}}
	l := computeUILayout(800, 500, 96, layoutStatus(), panel, fixedMeasure, fixedMeasure)
	pad := int32(uiPadDIP)
	inset := int32(uiCardPadDIP)

	// 日志区的控件让位：面板打开时没有过滤框与日志控件
	if l.Filter.Right > l.Filter.Left || l.Edit.Right > l.Edit.Left {
		t.Errorf("面板打开时不该有日志控件几何：filter=%+v edit=%+v", l.Filter, l.Edit)
	}
	// 输入框与添加按钮同一行、同一高度，且都落在卡片内边距里
	if l.SiteInput.Left != l.Card.Left+1+inset || l.SiteInput.Right >= l.Card.Right-1-inset {
		t.Errorf("站点输入框位置不对：%+v card=%+v", l.SiteInput, l.Card)
	}
	if got := l.SiteInput.Bottom - l.SiteInput.Top; got != int32(uiControlDIP) {
		t.Errorf("输入框高度应为 %d，实际 %d", uiControlDIP, got)
	}
	var add uiRegion
	found := false
	for _, r := range l.Regions {
		if r.ID == uiIDSiteAdd {
			add, found = r, true
		}
	}
	if !found {
		t.Fatalf("面板里应有「添加」按钮：%+v", l.Regions)
	}
	if add.Rect.Left <= l.SiteInput.Right || add.Rect.Top != l.SiteInput.Top {
		t.Errorf("「添加」应在输入框右侧且同一行：input=%+v add=%+v", l.SiteInput, add)
	}
	if add.Rect.Right != l.Card.Right-1-inset {
		t.Errorf("「添加」应贴右侧内边距：right=%d 期望 %d", add.Rect.Right, l.Card.Right-1-inset)
	}

	// 两行站点：每行有「删除」与「反代」，域名文字在左，反代胶囊的开关态来自列表数据
	if len(l.SiteRows) != 2 {
		t.Fatalf("应有两行站点，实际 %+v", l.SiteRows)
	}
	for i, row := range l.SiteRows {
		if row.Pattern != panel.Sites[i].Pattern {
			t.Errorf("第 %d 行域名不对：%q", i, row.Pattern)
		}
		if row.Rect.Top < l.SiteList.Top || row.Rect.Bottom > l.SiteList.Bottom {
			t.Errorf("第 %d 行超出列表区域：row=%+v list=%+v", i, row.Rect, l.SiteList)
		}
		if row.TextRight <= row.Rect.Left || row.TextRight > row.Rect.Right {
			t.Errorf("第 %d 行文字宽度不对：%+v", i, row)
		}
	}
	del0, ok := regionByID(l, uiSiteDelID(panel.Sites[0].Pattern))
	if !ok {
		t.Fatalf("第一行应有「删除」：%+v", l.Regions)
	}
	mitm0, ok := regionByID(l, uiSiteMITMID(panel.Sites[0].Pattern))
	if !ok {
		t.Fatalf("第一行应有「反代」：%+v", l.Regions)
	}
	if !mitm0.On {
		t.Error("第一行反代开着，胶囊应显示为开")
	}
	if mitm1, ok := regionByID(l, uiSiteMITMID(panel.Sites[1].Pattern)); !ok || mitm1.On {
		t.Errorf("第二行反代关着，胶囊应显示为关：%+v ok=%v", mitm1, ok)
	}
	// 行内控件：反代在删除左边、不重叠，且都不压到域名文字
	if mitm0.Rect.Right > del0.Rect.Left {
		t.Errorf("反代胶囊与删除按钮重叠：%+v vs %+v", mitm0.Rect, del0.Rect)
	}
	if l.SiteRows[0].TextRight > mitm0.Rect.Left {
		t.Errorf("域名文字压到了反代胶囊：%+v vs %+v", l.SiteRows[0], mitm0.Rect)
	}
	if del0.Rect.Right != l.Card.Right-1-inset {
		t.Errorf("「删除」应贴右侧内边距：right=%d 期望 %d", del0.Rect.Right, l.Card.Right-1-inset)
	}
	// 两行不重叠
	if l.SiteRows[1].Rect.Top < l.SiteRows[0].Rect.Bottom {
		t.Errorf("两行站点重叠：%+v / %+v", l.SiteRows[0].Rect, l.SiteRows[1].Rect)
	}

	// 底部说明在列表下面，且贴卡片下内边距
	if l.SiteHint.Top < l.SiteList.Bottom {
		t.Errorf("底部说明应在列表下方：hint=%+v list=%+v", l.SiteHint, l.SiteList)
	}
	if l.SiteHint.Bottom != 500-pad-inset {
		t.Errorf("底部说明应贴下内边距：bottom=%d 期望 %d", l.SiteHint.Bottom, 500-pad-inset)
	}

	// 命中判定与几何同源：每行的两个动作都能点中
	for _, r := range l.Regions {
		if r.Kind != uiGhost && r.Kind != uiChip {
			continue
		}
		got, ok := l.hit((r.Rect.Left+r.Rect.Right)/2, (r.Rect.Top+r.Rect.Bottom)/2)
		if !ok || got.ID != r.ID {
			t.Errorf("区域 %q 的中心点应命中自己，实际 %v/%q", r.ID, ok, got.ID)
		}
	}
}

// TestUILayoutSitePanelEmpty 空列表：仍然有输入框与添加按钮，行区域为空（绘制层画空状态提示）。
func TestUILayoutSitePanelEmpty(t *testing.T) {
	l := computeUILayout(800, 500, 96, layoutStatus(), uiPanelState{Open: true}, fixedMeasure, fixedMeasure)
	if len(l.SiteRows) != 0 {
		t.Errorf("没有站点时不该有行：%+v", l.SiteRows)
	}
	if l.SiteInput.Right <= l.SiteInput.Left {
		t.Errorf("空列表也要能输入：%+v", l.SiteInput)
	}
	if _, ok := regionByID(l, uiIDSiteAdd); !ok {
		t.Error("空列表也要有「添加」按钮")
	}
	if l.SiteList.Bottom <= l.SiteList.Top {
		t.Errorf("空状态提示也要有地方画：%+v", l.SiteList)
	}
}

// TestUILayoutSitePanelNarrow 窄窗口：输入框让路，但「添加」仍要在（面板的核心动作）。
func TestUILayoutSitePanelNarrow(t *testing.T) {
	l := computeUILayout(300, 320, 96, layoutStatus(), uiPanelState{Open: true}, fixedMeasure, fixedMeasure)
	add, ok := regionByID(l, uiIDSiteAdd)
	if !ok {
		t.Fatalf("窄窗口下仍应有「添加」：%+v", l.Regions)
	}
	if l.SiteInput.Right <= l.SiteInput.Left {
		t.Errorf("窄窗口下输入框仍要可用：%+v", l.SiteInput)
	}
	if add.Rect.Left < l.SiteInput.Right {
		t.Errorf("输入框与「添加」重叠：input=%+v add=%+v", l.SiteInput, add.Rect)
	}
	if l.SiteHint.Bottom > 320 {
		t.Errorf("底部说明超出客户区：%+v", l.SiteHint)
	}
}

// TestUILayoutSitePanelDropsOverflowRows 行数放不下时只画能放下的行（面板不做滚动）。
func TestUILayoutSitePanelDropsOverflowRows(t *testing.T) {
	sites := make([]uiSite, 0, 40)
	for i := 0; i < 40; i++ {
		sites = append(sites, uiSite{Pattern: "site" + string(rune('a'+i%26)) + ".com"})
	}
	l := computeUILayout(800, 400, 96, layoutStatus(), uiPanelState{Open: true, Sites: sites}, fixedMeasure, fixedMeasure)
	if len(l.SiteRows) >= len(sites) {
		t.Fatalf("放不下的行不该全部出现：%d / %d", len(l.SiteRows), len(sites))
	}
	for _, row := range l.SiteRows {
		if row.Rect.Bottom > l.SiteList.Bottom {
			t.Errorf("行超出列表区域：%+v list=%+v", row.Rect, l.SiteList)
		}
	}
}

// regionByID 从几何里找一块区域（测试用）。
func regionByID(l uiLayout, id string) (uiRegion, bool) {
	for _, r := range l.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return uiRegion{}, false
}
