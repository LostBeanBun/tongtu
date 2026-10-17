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
}

// windowStatusItem 是一个带状态点的开关项。
type windowStatusItem struct {
	Label string
	On    bool
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
	return b.String()
}
