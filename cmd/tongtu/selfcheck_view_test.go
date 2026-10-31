//go:build windows

package main

import (
	"strings"
	"testing"

	"tongtu/internal/selfcheck"
)

// 自检的渲染层是纯函数：把 selfcheck.Row 变成摘要行 / 一行反馈。
// 逻辑本身在 internal/selfcheck 里单测（含结论判定），这里只锁「呈现得对不对」。

func TestSelfCheckRowMarks(t *testing.T) {
	rows := selfCheckRows([]selfcheck.Row{
		{Host: "github.com", DNSOK: true, ProxyOK: true, ProxyTested: true, Verdict: selfcheck.VerdictOK},
		{Host: "raw.githubusercontent.com", DNSOK: true, DirectOK: true, ProxyTested: true, Verdict: selfcheck.VerdictDirectOnly},
		{Host: "store.steampowered.com", Verdict: selfcheck.VerdictFail},
	})
	// 三个域名各一行 + 一行「合计」
	if len(rows) != 4 {
		t.Fatalf("应为每个域名一行再加合计，实际 %d 行", len(rows))
	}
	if rows[0].Mark != markOK {
		t.Errorf("可用的域名应为 ✓，实际 %v", rows[0].Mark)
	}
	if rows[1].Mark != markWarn {
		t.Errorf("仅直连（没走加速）应提示警告，实际 %v", rows[1].Mark)
	}
	if rows[2].Mark != markWarn {
		t.Errorf("不通应为警告，实际 %v", rows[2].Mark)
	}
	if rows[3].Label != "合计" || !strings.Contains(rows[3].Value, "1 不通") {
		t.Errorf("最后一行应是合计：%+v", rows[3])
	}
}

func TestSelfCheckRowEmpty(t *testing.T) {
	if rows := selfCheckRows(nil); len(rows) != 0 {
		t.Errorf("没有结果时不该产出摘要行，实际 %+v", rows)
	}
}

func TestSelfCheckToast(t *testing.T) {
	ok := []selfcheck.Row{{Host: "a", Verdict: selfcheck.VerdictOK}}
	if text, style := selfCheckToast(ok); style != styleOK || !strings.Contains(text, "自检通过") {
		t.Errorf("全部可用应给成功反馈，实际 %q / %v", text, style)
	}
	bad := []selfcheck.Row{{Host: "a", Verdict: selfcheck.VerdictFail}}
	if text, style := selfCheckToast(bad); style != styleWarn || !strings.Contains(text, "不通") {
		t.Errorf("有不通时应给警告反馈，实际 %q / %v", text, style)
	}
	partial := []selfcheck.Row{{Host: "a", Verdict: selfcheck.VerdictDirectOnly}}
	if text, style := selfCheckToast(partial); style != styleWarn || !strings.Contains(text, "仅直连") {
		t.Errorf("仅直连要说明「没走加速」，实际 %q / %v", text, style)
	}
	if text, style := selfCheckToast(nil); style != styleWarn || !strings.Contains(text, "没有结果") {
		t.Errorf("零结果应是警告，实际 %q / %v", text, style)
	}
}

// TestTodoButtonAppearsWithCount 锁住「有待办才出现按钮」：没问题时界面不该多一个让人猜的按钮。
func TestTodoButtonAppearsWithCount(t *testing.T) {
	st := layoutStatus()
	st.TodoN = 0
	for _, b := range uiActionButtons(st) {
		if b.ID == uiIDTodo {
			t.Fatal("没有待办时不该出现「待办」按钮")
		}
	}

	st.TodoN = 2
	var todo *uiRegion
	for i := range uiActionButtons(st) {
		if b := uiActionButtons(st)[i]; b.ID == uiIDTodo {
			todo = &b
		}
	}
	if todo == nil {
		t.Fatal("有待办时应出现「待办」按钮")
	}
	if !strings.Contains(todo.Label, "2") {
		t.Errorf("按钮上应带待办数量，实际 %q", todo.Label)
	}
}

// TestTodoButtonIsLeftmost 锁住排布：待办最靠左（最需要处理的事离详情最近），退出最靠右。
func TestTodoButtonIsLeftmost(t *testing.T) {
	st := layoutStatus()
	st.TodoN = 1
	l := computeUILayout(800, 500, 96, st, uiPanelState{}, fixedMeasure, fixedMeasure)
	left, right := int32(1<<30), int32(-1)
	var leftID, rightID string
	for _, r := range l.Regions {
		if r.Kind != uiGhost || r.ID == uiIDOnlyWarn {
			continue // 只看警告在工具条里，不参与第二行的左右排布
		}
		if r.Rect.Left < left {
			left, leftID = r.Rect.Left, r.ID
		}
		if r.Rect.Right > right {
			right, rightID = r.Rect.Right, r.ID
		}
	}
	if leftID != uiIDTodo {
		t.Errorf("最左侧应是「待办」，实际 %s", leftID)
	}
	if rightID != uiIDQuit {
		t.Errorf("最右侧应是「退出」，实际 %s", rightID)
	}
}
