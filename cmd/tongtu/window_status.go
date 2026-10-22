package main

import (
	"strconv"
	"strings"
)

// windowStateLevel 表示运行健康度：决定状态词用哪种语义色（见 uiTheme.stateColor）。
type windowStateLevel int

const (
	stateOK       windowStateLevel = iota // 期望接管的都已生效
	stateDegraded                         // 有开关没生效（例如非管理员下 hosts 接管失败）
	stateOff                              // 服务未在运行
)

// modeWish 是一项「期望开 / 实际开」的对比，用于判断健康度。
type modeWish struct {
	want bool // 配置里期望接管
	have bool // 实际接管成功
}

// statusLevel 由「是否在跑」与各开关的期望/实际决定健康度。
//
// 这样界面能把「部分生效」自己说出来：例如系统代理成功了、hosts 因为不是管理员而失败，
// 状态词就用降级色，而不是一律显示「运行中」把问题藏起来。
func statusLevel(running bool, modes ...modeWish) windowStateLevel {
	if !running {
		return stateOff
	}
	for _, m := range modes {
		if m.want && !m.have {
			return stateDegraded
		}
	}
	return stateOK
}

// windowStatus 是窗口顶部状态条要展示的内容（跨平台共用，Windows 上由窗口自绘）。
//
// 状态条不用 STATIC 控件而是自己绘制：这样状态点、状态词的颜色（按健康度）和文字才能
// 精确按配色方案落笔（见 window_windows.go 的 paintHeader）。
type windowStatus struct {
	Title  string           // 左侧主标题，例如「通途 Tongtu 0.1.0」
	State  string           // 紧随标题的状态词，例如「运行中」
	Level  windowStateLevel // 状态词的健康度（决定颜色）
	Items  []windowStatusItem
	Detail []string // 第二行的信息片段（服务组 / 活跃隧道 / 流量），绘制时用「·」连接
	// TodoN 是待办项数量：> 0 时第二行会多出一个「待办 N」按钮（见 ui_layout_windows.go）。
	TodoN int
}

// windowStatusItem 是一个带状态点的开关项。
//
// Action 非空表示这一项在窗口里可以点击（值取 uiIDSys 等动作 id）——状态条因此不只是
// 展示，而是最短路径的开关入口：看到的和点的是同一个东西。
type windowStatusItem struct {
	Label  string
	On     bool
	Action string // 可点击的动作 id；空 = 只读展示
	Tip    string // 悬浮说明（与托盘菜单文案保持一致）
}

// modeItems 返回状态条上的三个加速模式开关项，状态还没准备好时用默认项：
// 窗口一创建就有可点的开关，几何计算也不依赖状态刷新时机。
func modeItems(items []windowStatusItem) []windowStatusItem {
	if len(items) > 0 {
		return items
	}
	return []windowStatusItem{
		{Label: "系统代理", Action: uiIDSys},
		{Label: "Hosts", Action: uiIDHosts},
		{Label: "无SNI反代", Action: uiIDMITM},
	}
}

// onOf 返回某个动作 id 当前是否处于开启状态（找不到该项时返回 false）。
func (s windowStatus) onOf(action string) bool {
	for _, it := range s.Items {
		if it.Action == action {
			return it.On
		}
	}
	return false
}

// key 返回状态的可比较指纹：相同则不必重绘。
func (s windowStatus) key() string {
	var b strings.Builder
	b.WriteString(s.Title)
	b.WriteByte('|')
	b.WriteString(s.State)
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(int(s.Level)))
	b.WriteByte('|')
	for _, it := range s.Items {
		b.WriteString(it.Label)
		if it.On {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte(',')
	}
	b.WriteByte('|')
	b.WriteString(strings.Join(s.Detail, "\x1f"))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(s.TodoN))
	return b.String()
}
