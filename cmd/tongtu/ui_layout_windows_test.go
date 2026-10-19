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
	l := computeUILayout(w, h, 96, layoutStatus(), fixedMeasure, fixedMeasure)
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
	btns := make([]uiRegion, 0, 4)
	for _, r := range l.Regions {
		if r.Kind == uiGhost {
			btns = append(btns, r)
		}
	}
	if len(btns) != 5 { // 退出 / 清空日志 / 打开配置 / 服务组 + 只看警告
		t.Fatalf("应有 5 个幽灵按钮，实际 %d", len(btns))
	}
	if btns[0].ID != uiIDQuit || btns[0].Rect.Right != w-pad {
		t.Errorf("最右侧应是「退出」并贴右留白：%+v", btns[0])
	}
	if btns[0].Rect.Top < int32(uiRow1DIP) {
		t.Errorf("动作按钮应在第二行：%+v", btns[0].Rect)
	}
	// 详情区不能压到按钮上
	firstBtn := btns[3] // 服务组（动作按钮表里的最后一个）
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
	l := computeUILayout(800, 500, 96, layoutStatus(), fixedMeasure, fixedMeasure)
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
	l := computeUILayout(180, 300, 96, layoutStatus(), fixedMeasure, fixedMeasure)
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
	l := computeUILayout(430, 300, 96, st, fixedMeasure, fixedMeasure)
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
	l := computeUILayout(700, 400, 96, windowStatus{}, fixedMeasure, fixedMeasure)
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
