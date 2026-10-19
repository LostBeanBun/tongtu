package main

// 窗口的交互层：鼠标悬停 / 点击、悬浮提示、轻量反馈、日志过滤与动作分发。
//
// 分工：几何在 ui_layout_windows.go（绘制与命中判定共用一份），本文件负责
// 「消息进来之后干什么」——所有函数都只在窗口线程（wndProc）上执行。

import (
	"log/slog"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// —— Win32 常量（只在本文件使用）——
const (
	wmCommand     = 0x0111
	wmTimer       = 0x0113
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmMouseLeave  = 0x02A3
	wmSetCursor   = 0x0020
	wmDestroy     = 0x0002

	enChange       = 0x0300 // EN_CHANGE（编辑框内容变化）
	emSetCueBanner = 0x1501 // EM_SETCUEBANNER：空内容时的灰色提示文字
	ecbTrue        = 1

	tmeLeave = 0x00000002 // TRACKMOUSEEVENT 的 TME_LEAVE
	idcHand  = 32649      // IDC_HAND

	srcCopy = 0x00CC0020 // BitBlt 的复制模式

	// 悬浮提示（tooltips_class32）
	ttmAddTool        = 0x0432 // TTM_ADDTOOLW
	ttmUpdateTipText  = 0x0439 // TTM_UPDATETIPTEXTW
	ttmSetMaxTipWidth = 0x0418 // TTM_SETMAXTIPWIDTH
	ttfIDIsHwnd       = 0x0001
	ttfSubclass       = 0x0010
	ttfTransparent    = 0x0100
	ttsAlwaysTip      = 0x01
	ttsNoPrefix       = 0x02
	wsPopup           = 0x80000000
	wsExTopmost       = 0x00000008
	swpNoMoveNoSize   = 0x0002 | 0x0001 // SWP_NOMOVE | SWP_NOSIZE
	diNormal          = 0x0003          // DrawIconEx：DI_NORMAL

	mfChecked = 0x0008
)

var tooltipsClass = utf16Ptr("tooltips_class32")

// toolInfoT 对应 TOOLINFOW（x64 下 64 字节）。
type toolInfoT struct {
	Size     uint32
	Flags    uint32
	Hwnd     windows.Handle
	ID       uintptr
	Rect     rectT
	Instance windows.Handle
	Text     *uint16
	LParam   uintptr
}

// trackMouseT 对应 TRACKMOUSEEVENT。
type trackMouseT struct {
	Size      uint32
	Flags     uint32
	HwndTrack windows.Handle
	HoverTime uint32
}

var (
	procSetCursor       = user32.NewProc("SetCursor")
	procScreenToClient  = user32.NewProc("ScreenToClient")
	procSetCapture      = user32.NewProc("SetCapture")
	procReleaseCapture  = user32.NewProc("ReleaseCapture")
	procTrackMouseEvent = user32.NewProc("TrackMouseEvent")
	procSetTimer        = user32.NewProc("SetTimer")
	procKillTimer       = user32.NewProc("KillTimer")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procDrawIconEx      = user32.NewProc("DrawIconEx")

	procBitBlt     = gdi32.NewProc("BitBlt")
	procNewMemDC   = gdi32.NewProc("CreateCompatibleDC")
	procNewMemBmp  = gdi32.NewProc("CreateCompatibleBitmap")
	procFreeDC     = gdi32.NewProc("DeleteDC")
	procFreeObject = gdi32.NewProc("DeleteObject")
)

// —— 鼠标与光标 ——

// onMouseMove 更新悬停高亮、手型光标与悬浮提示（都在同一份几何上判定）。
func (w *logWindow) onMouseMove(hwnd windows.Handle, lparam uintptr) {
	x, y := mousePoint(lparam)
	w.trackMouseLeave(hwnd)

	r, ok := w.regionAt(x, y)
	id := ""
	if ok {
		id = r.ID
	}
	if id == w.hoverID {
		return
	}
	w.hoverID = id
	w.updateTooltip(hwnd, ok, r)
	procInvalidateRect.Call(uintptr(hwnd), 0, 0)
}

// onMouseLeave 清掉悬停状态（鼠标移出客户区或移到子控件上）。
func (w *logWindow) onMouseLeave(hwnd windows.Handle) {
	if w.hoverID == "" {
		return
	}
	w.hoverID = ""
	w.updateTooltip(hwnd, false, uiRegion{})
	procInvalidateRect.Call(uintptr(hwnd), 0, 0)
}

// trackMouseLeave 请求一次 WM_MOUSELEAVE（每次移动都要重新请求）。
func (w *logWindow) trackMouseLeave(hwnd windows.Handle) {
	tme := trackMouseT{Size: uint32(unsafe.Sizeof(trackMouseT{})), Flags: tmeLeave, HwndTrack: hwnd}
	procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
}

// onSetCursor 在可点击区域上显示手型光标，其余交回系统（箭头）。
func (w *logWindow) onSetCursor(hwnd windows.Handle, lparam uintptr) bool {
	if lparam&0xFFFF != 1 { // 1 = HTCLIENT
		return false
	}
	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procScreenToClient.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pt)))
	if _, ok := w.regionAt(pt.X, pt.Y); !ok {
		return false
	}
	if hand, _, _ := procLoadCursorW.Call(0, idcHand); hand != 0 {
		procSetCursor.Call(hand)
	}
	return true
}

// onLButtonDown 记录按下的区域并捕获鼠标（拖出按钮再松开就等于取消）。
func (w *logWindow) onLButtonDown(hwnd windows.Handle, lparam uintptr) {
	x, y := mousePoint(lparam)
	r, ok := w.regionAt(x, y)
	if !ok {
		return
	}
	w.pressID = r.ID
	procSetCapture.Call(uintptr(hwnd))
	procInvalidateRect.Call(uintptr(hwnd), 0, 0)
}

// onLButtonUp 在同一区域松开才触发动作（这是按钮的标准手感）。
func (w *logWindow) onLButtonUp(hwnd windows.Handle, lparam uintptr) {
	if w.pressID == "" {
		return
	}
	id := w.pressID
	w.pressID = ""
	procReleaseCapture.Call()
	procInvalidateRect.Call(uintptr(hwnd), 0, 0)

	x, y := mousePoint(lparam)
	r, ok := w.regionAt(x, y)
	if !ok || r.ID != id {
		return // 松手时已经不在这个按钮上：取消
	}
	w.invokeAction(hwnd, id)
}

// mousePoint 从 lparam 取出鼠标客户区坐标（低 16 位 x、高 16 位 y，带符号）。
func mousePoint(lparam uintptr) (int32, int32) {
	return int32(int16(lparam & 0xFFFF)), int32(int16((lparam >> 16) & 0xFFFF))
}

// —— 悬浮提示 ——

// createTooltip 建一个跟随本窗口的提示条（失败就算了，界面其余部分照常工作）。
func (w *logWindow) createTooltip(hwnd windows.Handle) {
	if tooltipsClass == nil {
		return
	}
	hInst, _, _ := procGetModuleHandleW.Call(0)
	tip, _, _ := procCreateWindowExW.Call(
		wsExTopmost,
		uintptr(unsafe.Pointer(tooltipsClass)),
		0,
		wsPopup|ttsAlwaysTip|ttsNoPrefix,
		0, 0, 0, 0,
		uintptr(hwnd), 0, hInst, 0,
	)
	if tip == 0 {
		return
	}
	procSendMessageW.Call(tip, ttmSetMaxTipWidth, 0, 360)

	ti := toolInfoT{
		Size:     uint32(unsafe.Sizeof(toolInfoT{})),
		Flags:    ttfIDIsHwnd | ttfSubclass | ttfTransparent,
		Hwnd:     hwnd,
		ID:       uintptr(hwnd),
		Instance: windows.Handle(hInst),
		Text:     nil, // 先空着：没有悬停目标时不弹
	}
	var r rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	ti.Rect = r
	procSendMessageW.Call(tip, ttmAddTool, 0, uintptr(unsafe.Pointer(&ti)))

	// 过滤框自己带一条固定说明（它是个子控件，鼠标消息不经过父窗口，因此单独挂）
	if filter := w.filterHandle(); filter != 0 {
		if text := utf16Ptr(uiFilterTip); text != nil {
			ft := toolInfoT{
				Size:     uint32(unsafe.Sizeof(toolInfoT{})),
				Flags:    ttfIDIsHwnd | ttfSubclass,
				Hwnd:     filter,
				ID:       uintptr(filter),
				Instance: windows.Handle(hInst),
				Text:     text,
			}
			procSendMessageW.Call(tip, ttmAddTool, 0, uintptr(unsafe.Pointer(&ft)))
		}
	}

	w.mu.Lock()
	w.tooltip = windows.Handle(tip)
	w.mu.Unlock()
}

// updateTooltip 把提示文字换成当前悬停区域的说明，并把提示范围收到该区域上；
// 没有悬停目标时置空——空的提示不会弹出来。
func (w *logWindow) updateTooltip(hwnd windows.Handle, ok bool, r uiRegion) {
	w.mu.Lock()
	tip := w.tooltip
	w.mu.Unlock()
	if tip == 0 {
		return
	}
	ti := toolInfoT{
		Size: uint32(unsafe.Sizeof(toolInfoT{})),
		Hwnd: hwnd,
		ID:   uintptr(hwnd),
	}
	if ok {
		ti.Rect = r.Rect
		if p := utf16Ptr(r.Tip); p != nil {
			ti.Text = p
		}
	}
	procSendMessageW.Call(uintptr(tip), ttmUpdateTipText, 0, uintptr(unsafe.Pointer(&ti)))
}

// —— 轻量反馈（toast）——

// toast 在状态条第二行短暂显示一条反馈。不弹模态框：切换开关这类操作不该打断用户。
func (w *logWindow) toast(text string, style logStyle) {
	if text == "" {
		return
	}
	hwnd := w.handle()
	w.mu.Lock()
	w.toastMsg = uiToast{Text: text, Style: style, Until: time.Now().Add(uiToastMS * time.Millisecond)}
	w.mu.Unlock()
	if hwnd != 0 {
		procSetTimer.Call(uintptr(hwnd), uiTimerToast, uiToastMS, 0)
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}
}

// onTimer 处理提示超时与过滤防抖。
func (w *logWindow) onTimer(hwnd windows.Handle, id uintptr) {
	switch id {
	case uiTimerToast:
		procKillTimer.Call(uintptr(hwnd), uiTimerToast)
		w.mu.Lock()
		w.toastMsg = uiToast{}
		w.mu.Unlock()
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	case uiTimerFilter:
		procKillTimer.Call(uintptr(hwnd), uiTimerFilter)
		w.applyFilter()
	}
}

// activeToast 返回当前仍有效的提示（已过期或被清掉时返回零值）。
func (w *logWindow) activeToast() uiToast {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.toastMsg.Text == "" || time.Now().After(w.toastMsg.Until) {
		return uiToast{}
	}
	return w.toastMsg
}

// —— 日志过滤 ——

// filterText 读取过滤框里的关键字。
func (w *logWindow) filterText() string {
	w.mu.Lock()
	filter := w.filter
	w.mu.Unlock()
	if filter == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	n, _, _ := procGetWindowTextW.Call(uintptr(filter), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return strings.TrimSpace(windows.UTF16ToString(buf[:n]))
}

// onFilterChanged 每次输入都重置防抖定时器：连续打字只在停手后重画一次。
func (w *logWindow) onFilterChanged(hwnd windows.Handle) {
	procSetTimer.Call(uintptr(hwnd), uiTimerFilter, uiFilterWaitMS, 0)
}

// scheduleFilter 请求一次（带防抖的）重画：过滤生效期间来了新日志时用。
func (w *logWindow) scheduleFilter() {
	hwnd := w.handle()
	if hwnd == 0 {
		return
	}
	if !w.filterActive() {
		return // 没过滤时新日志照常追加，不必整体重画
	}
	procSetTimer.Call(uintptr(hwnd), uiTimerFilter, uiFilterWaitMS, 0)
}

// filterActive 报告当前是否有过滤条件（关键字或只看警告）。
func (w *logWindow) filterActive() bool {
	w.mu.Lock()
	only := w.onlyWarn
	w.mu.Unlock()
	return only || w.filterText() != ""
}

// applyFilter 按当前条件重画日志区（没有任何日志时给出空状态提示）。
func (w *logWindow) applyFilter() {
	query, only := w.filterText(), w.onlyWarnValue()
	w.mu.Lock()
	lines := append([]logLine(nil), w.lines...)
	w.mu.Unlock()

	visible := filterLines(lines, query, only)
	if len(visible) == 0 && (query != "" || only) {
		visible = []logLine{{Spans: []logSpan{{Style: styleMuted, Text: uiNoMatchHint}}}}
	}
	w.renderLines(visible)
}

// setOnlyWarn 切换「只看警告」并立即重画（用户明确点了，不必防抖）。
func (w *logWindow) setOnlyWarn(on bool) {
	w.mu.Lock()
	w.onlyWarn = on
	w.mu.Unlock()
	w.applyFilter()
	hwnd := w.handle()
	if hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0) // 让按钮的选中态跟着变
	}
}

// onlyWarnValue 读取「只看警告」的当前状态。
func (w *logWindow) onlyWarnValue() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.onlyWarn
}

// clearLog 清空窗口里的日志（日志文件与控制台不受影响）。
func (w *logWindow) clearLog() {
	w.mu.Lock()
	w.lines = nil
	w.lineBytes = 0
	w.mu.Unlock()
	setEditText(w.editHandle(), "")
}

// —— 动作分发 ——

// uiModeNames 是开关项在反馈文案里的名字。
var uiModeNames = map[string]string{
	uiIDSys:   "系统代理",
	uiIDHosts: "Hosts",
	uiIDMITM:  "无SNI反代",
}

// invokeAction 执行一次界面动作：成功的给轻量反馈，失败的写日志 + 提示原因。
func (w *logWindow) invokeAction(hwnd windows.Handle, id string) {
	switch id {
	case uiIDClear:
		w.clearLog()
		w.toast("日志已清空", styleMuted)
		return
	case uiIDOnlyWarn:
		w.setOnlyWarn(!w.onlyWarnValue())
		return
	}

	act := currentWindowActions()
	switch id {
	case uiIDSys, uiIDHosts, uiIDMITM:
		if act.ToggleMode == nil {
			w.toast("当前实例没有可用的开关实现", styleWarn)
			return
		}
		w.toggleMode(act, id)
	case uiIDServices:
		w.showServicesMenu(hwnd)
	case uiIDConfig:
		if act.OpenConfig == nil {
			w.toast("当前实例无法打开配置文件", styleWarn)
			return
		}
		if err := act.OpenConfig(); err != nil {
			w.report(id, err)
		}
	case uiIDQuit:
		if act.Quit != nil {
			act.Quit()
		}
	}
}

// toggleMode 切换单个加速模式。开关的当前状态取自状态条（与显示完全一致）。
func (w *logWindow) toggleMode(act windowActions, id string) {
	on := !w.statusSnapshot().onOf(id)
	if err := act.ToggleMode(id, on); err != nil {
		w.report(id, err)
		return
	}
	word := "已关闭"
	if on {
		word = "已开启"
	}
	w.toast(uiModeNames[id]+word, styleOK)
	if act.Refresh != nil {
		act.Refresh() // 立刻回读真实状态，不等下一轮 3 秒轮询
	}
}

// report 把失败写进日志（窗口里能看到完整原因），并在状态条上给一行简短提示。
func (w *logWindow) report(id string, err error) {
	slog.Warn("界面操作失败", "action", id, "err", err)
	w.toast(err.Error(), styleWarn)
}

// showServicesMenu 在「服务组」按钮下弹出可勾选的服务组菜单（与托盘同一套开关）。
func (w *logWindow) showServicesMenu(hwnd windows.Handle) {
	act := currentWindowActions()
	if act.Services == nil || act.SetServices == nil {
		w.toast("当前实例没有可选的服务组", styleWarn)
		return
	}
	svcs := act.Services()
	if len(svcs) == 0 {
		w.toast("当前实例没有可选的服务组", styleWarn)
		return
	}

	// 菜单锚在按钮的左下角（找不到按钮就锚在鼠标位置）
	r, ok := w.regionAtID(uiIDServices)
	var pt pointT
	if ok {
		pt = pointT{X: r.Rect.Left, Y: r.Rect.Bottom}
	} else {
		procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		procScreenToClient.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pt)))
	}

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	const base = 100
	enabled := map[int]string{}
	for i, s := range svcs {
		flags := uintptr(mfString)
		if s.On {
			flags |= mfChecked
		}
		label := s.Name
		if !strings.HasPrefix(label, "加速") {
			label = "加速 " + label
		}
		appendMenuItem(menu, flags, uintptr(base+i), label)
		enabled[base+i] = s.ID
	}

	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	id, ok := enabled[int(int32(cmd))]
	if !ok {
		return // 用户点了空白处 / 按了 Esc
	}
	next := make([]string, 0, len(svcs)+1)
	for _, s := range svcs {
		if s.On && s.ID != id {
			next = append(next, s.ID)
		}
	}
	on := false
	for _, s := range svcs {
		if s.ID == id {
			on = !s.On
		}
	}
	if on {
		next = append(next, id)
	}
	if err := act.SetServices(next); err != nil {
		w.report(uiIDServices, err)
		return
	}
	word := "已关闭"
	if on {
		word = "已开启"
	}
	w.toast("服务组 "+id+" "+word, styleOK)
	if act.Refresh != nil {
		act.Refresh()
	}
}

// regionAtID 按动作 id 找当前几何里的区域（找不到返回 false）。
func (w *logWindow) regionAtID(id string) (uiRegion, bool) {
	for _, r := range w.ensureLayout().Regions {
		if r.ID == id {
			return r, true
		}
	}
	return uiRegion{}, false
}
