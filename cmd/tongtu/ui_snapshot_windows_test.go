//go:build windows

package main

import "testing"

// TestUILayoutSnapshot 覆盖给控制端口 /ui/layout 用的几何快照：
// 没画过窗口时是 nil，画过之后能按 ID 取到矩形，并且暂停后同一个按钮的标签会变成「恢复」。
// （真机脚本就是靠这份数据点按钮的；标签错了脚本会点到「恢复」却以为在「暂停」。）
func TestUILayoutSnapshot(t *testing.T) {
	uiSnapMu.Lock()
	old := uiSnap
	uiSnap = nil
	uiSnapMu.Unlock()
	t.Cleanup(func() {
		uiSnapMu.Lock()
		uiSnap = old
		uiSnapMu.Unlock()
	})

	if got := uiLayoutSnapshot(); got != nil {
		t.Fatalf("还没画过窗口时快照应为 nil，实际 %#v", got)
	}

	st := layoutStatus()
	l := computeUILayout(855, 481, 96, st, uiPanelState{}, fixedMeasure, fixedMeasure)
	recordUILayout(l, 855, 481, st)

	snap, ok := uiLayoutSnapshot().(uiSnapshot)
	if !ok {
		t.Fatalf("快照类型不符: %#v", uiLayoutSnapshot())
	}
	if snap.ClientW != 855 || snap.ClientH != 481 || snap.DPI != 96 {
		t.Errorf("客户区/DPI 不符: %dx%d @%d", snap.ClientW, snap.ClientH, snap.DPI)
	}
	if snap.Paused {
		t.Error("未暂停时快照不应标记为暂停")
	}
	if len(snap.Regions) != len(l.Regions) {
		t.Fatalf("区域数量不符: 快照 %d，布局 %d", len(snap.Regions), len(l.Regions))
	}

	findPause := func(s uiSnapshot) (uiRegionSnapshot, bool) {
		for _, r := range s.Regions {
			if r.ID == uiIDPause {
				return r, true
			}
		}
		return uiRegionSnapshot{}, false
	}
	pause, ok := findPause(snap)
	if !ok {
		t.Fatal("快照里应包含暂停按钮（id=pause）")
	}
	if pause.Label != "暂停" {
		t.Errorf("运行中时暂停按钮标签 = %q，期望「暂停」", pause.Label)
	}
	if pause.Right <= pause.Left || pause.Bottom <= pause.Top {
		t.Errorf("按钮矩形不合法: %+v", pause)
	}

	// 暂停后重新布局：同一个 ID 的标签必须变成「恢复」，其余区域不得为负宽度。
	st.Paused = true
	st.Items = nil
	l2 := computeUILayout(855, 481, 96, st, uiPanelState{}, fixedMeasure, fixedMeasure)
	recordUILayout(l2, 855, 481, st)
	snap2 := uiLayoutSnapshot().(uiSnapshot)
	if !snap2.Paused {
		t.Error("暂停后快照应标记 Paused")
	}
	pause2, ok := findPause(snap2)
	if !ok {
		t.Fatal("暂停后快照里仍应包含暂停/恢复按钮")
	}
	if pause2.Label != "恢复" {
		t.Errorf("暂停后按钮标签 = %q，期望「恢复」", pause2.Label)
	}
	for _, r := range snap2.Regions {
		if r.Right < r.Left || r.Bottom < r.Top {
			t.Errorf("区域矩形为负: %+v", r)
		}
	}
}

// TestUILayoutSnapshotSitesPanel 面板打开时快照要标出来（日志控件已隐藏），并带上站点列表：
// 真机脚本据此判断当前该点哪一套控件，而不是照着旧快照点隐藏的日志区。
func TestUILayoutSnapshotSitesPanel(t *testing.T) {
	uiSnapMu.Lock()
	old := uiSnap
	uiSnapMu.Unlock()
	t.Cleanup(func() {
		uiSnapMu.Lock()
		uiSnap = old
		uiSnapMu.Unlock()
	})

	st := layoutStatus()
	panel := uiPanelState{Open: true, Sites: []uiSite{{Pattern: "a.com", MITM: true}, {Pattern: "b.com"}}}
	l := computeUILayout(855, 481, 96, st, panel, fixedMeasure, fixedMeasure)
	recordUILayout(l, 855, 481, st)

	snap := uiLayoutSnapshot().(uiSnapshot)
	if !snap.SitesOpen {
		t.Error("面板打开时快照应标记 sites_open")
	}
	if len(snap.Sites) != 2 || snap.Sites[0] != "a.com" || snap.Sites[1] != "b.com" {
		t.Errorf("快照里的站点列表不对：%v", snap.Sites)
	}

	// 回到日志模式：标记消失，站点列表清空
	l2 := computeUILayout(855, 481, 96, st, uiPanelState{}, fixedMeasure, fixedMeasure)
	recordUILayout(l2, 855, 481, st)
	snap2 := uiLayoutSnapshot().(uiSnapshot)
	if snap2.SitesOpen || len(snap2.Sites) != 0 {
		t.Errorf("日志模式下不该标记面板：%+v", snap2)
	}
}
