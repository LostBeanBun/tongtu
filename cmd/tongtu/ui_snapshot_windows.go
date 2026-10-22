//go:build windows

package main

// 界面几何快照：给控制端口 /ui/layout 用。
//
// 为什么需要它：窗口是 GDI 自绘的，按钮没有 UI Automation 信息，真机脚本想「点准按钮」
// 只能靠猜像素——而状态条上的流量计数每秒都在变，悬停求矩形很容易被这些变化带偏
// （0.4.0 的冒烟脚本就因此点到了空白处）。这里把每帧算好的几何原样暴露出来：
// 只读、仅回环，不参与任何加速逻辑，作用相当于给自绘界面加一份「无障碍树」。
//
// 真机脚本按 ID 取矩形（例如 id=pause），点击它的中心；顺带还能断言
// 「暂停后同一个按钮的标签变成恢复」——那正是 0.3.0 里 layoutKey 漏算 Paused 的回归点。

import (
	"sync"
)

// uiRegionSnapshot 是一块可交互区域的矩形（客户区坐标，像素）。
type uiRegionSnapshot struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Left   int32  `json:"left"`
	Top    int32  `json:"top"`
	Right  int32  `json:"right"`
	Bottom int32  `json:"bottom"`
}

// uiSnapshot 是最近一帧的界面几何。
type uiSnapshot struct {
	DPI     uint32             `json:"dpi"`
	ClientW int32              `json:"client_w"`
	ClientH int32              `json:"client_h"`
	Paused  bool               `json:"paused"`
	Regions []uiRegionSnapshot `json:"regions"`
}

var (
	uiSnapMu sync.Mutex
	uiSnap   *uiSnapshot
)

// recordUILayout 记下刚刚算出的几何。只在几何重算时调用（见 ensureLayout）。
func recordUILayout(l uiLayout, clientW, clientH int32, st windowStatus) {
	regions := make([]uiRegionSnapshot, 0, len(l.Regions))
	for _, r := range l.Regions {
		regions = append(regions, uiRegionSnapshot{
			ID: r.ID, Label: r.Label,
			Left: r.Rect.Left, Top: r.Rect.Top, Right: r.Rect.Right, Bottom: r.Rect.Bottom,
		})
	}
	snap := &uiSnapshot{DPI: l.DPI, ClientW: clientW, ClientH: clientH, Paused: st.Paused, Regions: regions}
	uiSnapMu.Lock()
	uiSnap = snap
	uiSnapMu.Unlock()
}

// uiLayoutSnapshot 返回最近一帧的几何；窗口还没画过时返回 nil。
func uiLayoutSnapshot() any {
	uiSnapMu.Lock()
	defer uiSnapMu.Unlock()
	if uiSnap == nil {
		return nil
	}
	return *uiSnap
}
