//go:build windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 本文件单测「交互层」：把鼠标消息真的发给窗口过程，断言动作被正确触发。
//
// 为什么值得这么测：命中判定、动作分发、反馈（轻量提示 / 日志 / 过滤）串在一起才是
// 用户看到的「点得动」，而它们各自都很容易在改动里悄悄错位（几何改了、id 写错了、
// 失败路径忘了给反馈）。这里用假的 windowActions 记录调用，不碰真实系统。

// fakeActions 记录界面触发过的动作。
type fakeActions struct {
	toggleCalls []struct {
		ID string
		On bool
	}
	toggleErr   error
	refreshHits int
	quitHits    int
	configHits  int
}

func (f *fakeActions) actions() windowActions {
	return windowActions{
		ToggleMode: func(id string, on bool) error {
			f.toggleCalls = append(f.toggleCalls, struct {
				ID string
				On bool
			}{id, on})
			return f.toggleErr
		},
		SetServices: func([]string) error { return nil },
		Services:    func() []uiService { return nil },
		OpenConfig:  func() error { f.configHits++; return nil },
		Quit:        func() { f.quitHits++ },
		Refresh:     func() { f.refreshHits++ },
	}
}

// useFakeActions 注入假动作，并在测试结束时清掉（避免影响其它测试）。
func useFakeActions(t *testing.T, f *fakeActions) {
	t.Helper()
	setWindowActionsHook(f.actions)
	t.Cleanup(func() { setWindowActionsHook(nil) })
}

// sendRegionMouse 把一条鼠标消息发给某个自绘区域的中心点。
func sendRegionMouse(t *testing.T, id string, msg uint32, wparam uintptr) {
	t.Helper()
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	if hwnd == 0 {
		t.Fatal("窗口不存在")
	}
	r, ok := lw.regionAtID(id)
	if !ok {
		t.Fatalf("当前几何里没有区域 %q", id)
	}
	x := (r.Rect.Left + r.Rect.Right) / 2
	y := (r.Rect.Top + r.Rect.Bottom) / 2
	sendMouse(t, hwnd, msg, wparam, x, y)
}

// sendMouse 把一条鼠标消息发给窗口指定客户区坐标。
func sendMouse(t *testing.T, hwnd windows.Handle, msg uint32, wparam uintptr, x, y int32) {
	t.Helper()
	lparam := uintptr(uint16(x)) | uintptr(uint16(y))<<16
	procSendMessageW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
}

// clickRegion 模拟一次完整点击（按下与抬起都落在同一个区域上）。
func clickRegion(t *testing.T, id string) {
	t.Helper()
	sendRegionMouse(t, id, wmLButtonDown, 1)
	sendRegionMouse(t, id, wmLButtonUp, 0)
}

// setStatusForInteraction 摆一份「三项都在、可直接点」的状态条内容。
func setStatusForInteraction(hosts, sys, mitm bool) {
	setWindowStatus(windowStatus{
		Title: "通途 Tongtu " + Version,
		State: "运行中",
		Level: stateOK,
		Items: []windowStatusItem{
			{Label: "系统代理", On: sys, Action: uiIDSys, Tip: uiModeTips[uiIDSys]},
			{Label: "Hosts", On: hosts, Action: uiIDHosts, Tip: uiModeTips[uiIDHosts]},
			{Label: "无SNI反代", On: mitm, Action: uiIDMITM, Tip: uiModeTips[uiIDMITM]},
		},
		Detail: []string{"服务组 Steam", "活跃隧道 1"},
	})
}

func noToast(t *testing.T) {
	t.Helper()
	lw.mu.Lock()
	lw.toastMsg = uiToast{}
	lw.mu.Unlock()
}

func TestClickToggleModeInvokesAction(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, true, true)
	f := &fakeActions{}
	useFakeActions(t, f)
	noToast(t)

	// Hosts 当前是关的：点一下应该请求「打开」，并给出「已开启」的反馈
	clickRegion(t, uiIDHosts)
	if len(f.toggleCalls) != 1 || f.toggleCalls[0].ID != uiIDHosts || !f.toggleCalls[0].On {
		t.Fatalf("点 Hosts 胶囊应请求开启 hosts，实际 %+v", f.toggleCalls)
	}
	if got := lw.activeToast(); got.Text != "Hosts已开启" || got.Style != styleOK {
		t.Errorf("成功后应给出轻量反馈，实际 %+v", got)
	}
	if f.refreshHits != 1 {
		t.Errorf("操作成功后应立即回读一次状态，实际 %d 次", f.refreshHits)
	}

	// 系统代理当前是开的：点一下应该请求「关闭」
	clickRegion(t, uiIDSys)
	if len(f.toggleCalls) != 2 || f.toggleCalls[1].ID != uiIDSys || f.toggleCalls[1].On {
		t.Fatalf("点系统代理胶囊应请求关闭，实际 %+v", f.toggleCalls)
	}
	if got := lw.activeToast().Text; got != "系统代理已关闭" {
		t.Errorf("关闭后的反馈文案不对：%q", got)
	}
}

func TestClickToggleModeKeepsOtherModeIntact(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	// 反转开关时不能把另一项一起改了：这里只要断言「只发一次请求、只针对被点的那一项」。
	setStatusForInteraction(true, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)

	clickRegion(t, uiIDMITM)
	if len(f.toggleCalls) != 1 || f.toggleCalls[0].ID != uiIDMITM || !f.toggleCalls[0].On {
		t.Fatalf("只应请求开启反代这一项，实际 %+v", f.toggleCalls)
	}
}

func TestClickCancelWhenReleasedOutside(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)

	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	r, ok := lw.regionAtID(uiIDQuit)
	if !ok {
		t.Fatal("几何里没有退出按钮")
	}
	// 在退出按钮上按下，但移到别处松开：这是「反悔」，不该触发任何动作
	sendMouse(t, hwnd, wmLButtonDown, 1, (r.Rect.Left+r.Rect.Right)/2, (r.Rect.Top+r.Rect.Bottom)/2)
	sendMouse(t, hwnd, wmLButtonUp, 0, 4, r.Rect.Bottom+40)
	if f.quitHits != 0 {
		t.Errorf("在别处松开不该退出，实际退出 %d 次", f.quitHits)
	}
	if lw.pressID != "" {
		t.Errorf("松开后按下状态应清空，实际 %q", lw.pressID)
	}
}

func TestClickBlankAreaDoesNothing(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)

	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	// 日志卡片中间是空白（没有自绘区域），点它什么都不该发生
	sendMouse(t, hwnd, wmLButtonDown, 1, cr.Right/2, cr.Bottom-30)
	sendMouse(t, hwnd, wmLButtonUp, 0, cr.Right/2, cr.Bottom-30)
	if len(f.toggleCalls) != 0 || f.quitHits != 0 || f.configHits != 0 {
		t.Errorf("点空白处不该触发动作：%+v", f)
	}
}

func TestToggleFailureReportsInToast(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, true, true)
	f := &fakeActions{toggleErr: errors.New("需要管理员权限")}
	useFakeActions(t, f)
	noToast(t)

	clickRegion(t, uiIDHosts)
	if got := lw.activeToast(); got.Text != "需要管理员权限" || got.Style != styleWarn {
		t.Errorf("失败时应把原因显示在状态条上，实际 %+v", got)
	}
	if f.refreshHits != 0 {
		t.Errorf("失败时不该回读状态（本来就没变），实际 %d 次", f.refreshHits)
	}
}

func TestClickQuitAndConfig(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)

	clickRegion(t, uiIDConfig)
	if f.configHits != 1 {
		t.Errorf("点「打开配置」应调用一次 OpenConfig，实际 %d", f.configHits)
	}
	clickRegion(t, uiIDQuit)
	if f.quitHits != 1 {
		t.Errorf("点「退出」应调用一次 Quit，实际 %d", f.quitHits)
	}
}

func TestQuitWithoutActionsDoesNothing(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	setWindowActionsHook(nil) // 未注入动作（例如窗口独立运行时）
	clickRegion(t, uiIDQuit)  // 不应崩
}

func TestClearLogButtonClearsEdit(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	useFakeActions(t, &fakeActions{})

	_, _ = lw.Write([]byte("清空之前的一行日志\n"))
	if got := editText(t); !strings.Contains(got, "清空之前的一行日志") {
		t.Fatalf("准备条件不成立：日志控件里没有刚写入的行：%q", got)
	}
	clickRegion(t, uiIDClear)
	if got := editText(t); got != "" {
		t.Errorf("点「清空日志」后日志控件应为空，实际 %q", got)
	}
	lw.mu.Lock()
	n := len(lw.lines)
	lw.mu.Unlock()
	if n != 0 {
		t.Errorf("清空后缓冲区也应为空，实际 %d 行", n)
	}
	if got := lw.activeToast().Text; got != "日志已清空" {
		t.Errorf("清空后应给出反馈，实际 %q", got)
	}
}

func TestHoverAndLeave(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.hoverID = ""
	lw.mu.Unlock()

	sendRegionMouse(t, uiIDMITM, wmMouseMove, 0)
	if lw.hoverID != uiIDMITM {
		t.Errorf("悬停在反代胶囊上时 hoverID 应为 %q，实际 %q", uiIDMITM, lw.hoverID)
	}
	// 移到空白处：悬停高亮与提示都要收掉
	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	sendMouse(t, hwnd, wmMouseMove, 0, cr.Right/2, cr.Bottom-30)
	if lw.hoverID != "" {
		t.Errorf("移到空白处应清掉悬停，实际 %q", lw.hoverID)
	}
	procSendMessageW.Call(uintptr(hwnd), wmMouseLeave, 0, 0)
	if lw.hoverID != "" {
		t.Errorf("鼠标离开后应清掉悬停，实际 %q", lw.hoverID)
	}
}

func TestOnlyWarnButtonFiltersLog(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	useFakeActions(t, &fakeActions{})
	lw.setOnlyWarn(false)
	_, _ = lw.Write([]byte("普通信息一行\n"))
	emitWarnLog(t, "写入 hosts 失败")

	clickRegion(t, uiIDOnlyWarn)
	if !lw.onlyWarnValue() {
		t.Fatal("点一次「只看警告」应打开过滤")
	}
	got := editText(t)
	if !strings.Contains(got, "写入 hosts 失败") {
		t.Errorf("只看警告时应保留警告行：%q", got)
	}
	if strings.Contains(got, "普通信息一行") {
		t.Errorf("只看警告时不该显示普通信息行：%q", got)
	}

	// 再点一次：恢复全部日志
	clickRegion(t, uiIDOnlyWarn)
	if lw.onlyWarnValue() {
		t.Fatal("再点一次应关闭过滤")
	}
	if got := editText(t); !strings.Contains(got, "普通信息一行") {
		t.Errorf("关掉过滤后应恢复全部日志：%q", got)
	}
}

func TestOnlyWarnWithNoMatchesShowsHint(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	useFakeActions(t, &fakeActions{})
	lw.clearLog()
	lw.setOnlyWarn(false)
	_, _ = lw.Write([]byte("只有普通信息\n"))

	clickRegion(t, uiIDOnlyWarn)
	got := editText(t)
	if !strings.Contains(got, uiNoMatchHint) {
		t.Errorf("没有命中时应显示提示行，实际 %q", got)
	}
	lw.setOnlyWarn(false)
}

func TestFilterBoxDebounceAppliesFilter(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	useFakeActions(t, &fakeActions{})
	lw.setOnlyWarn(false)
	filter := lw.filterHandle()
	if filter == 0 {
		t.Skip("这个窗口宽度下没有过滤框（子控件未创建）")
	}
	lw.clearLog()
	_, _ = lw.Write([]byte("隧道已建立 steamcommunity.com\n"))
	_, _ = lw.Write([]byte("服务组已加载 groups=[steam github]\n"))

	setEditText(filter, "STEAMCOMMUNITY") // 关键字不区分大小写
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	procSendMessageW.Call(uintptr(hwnd), wmTimer, uiTimerFilter, 0) // 模拟防抖定时器到点

	got := editText(t)
	if !strings.Contains(got, "steamcommunity.com") {
		t.Errorf("命中关键字的行应保留：%q", got)
	}
	if strings.Contains(got, "groups=[steam github]") {
		t.Errorf("没命中关键字的行应被过滤：%q", got)
	}

	setEditText(filter, "")
	procSendMessageW.Call(uintptr(hwnd), wmTimer, uiTimerFilter, 0)
	if got := editText(t); !strings.Contains(got, "groups=[steam github]") {
		t.Errorf("清空关键字后应恢复全部日志：%q", got)
	}
}

func TestToastExpiresOnTimer(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.toast("测试提示", styleOK)
	if got := lw.activeToast(); got.Text != "测试提示" {
		t.Fatalf("刚发出的提示应当有效，实际 %+v", got)
	}
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	procSendMessageW.Call(uintptr(hwnd), wmTimer, uiTimerToast, 0)
	if got := lw.activeToast(); got.Text != "" {
		t.Errorf("定时器到点后提示应消失，实际 %+v", got)
	}
}

func TestActiveToastIgnoresExpired(t *testing.T) {
	lw.mu.Lock()
	lw.toastMsg = uiToast{Text: "早就过期了", Style: styleOK, Until: time.Now().Add(-time.Second)}
	lw.mu.Unlock()
	if got := lw.activeToast(); got.Text != "" {
		t.Errorf("过期的提示不该再显示，实际 %+v", got)
	}
	lw.mu.Lock()
	lw.toastMsg = uiToast{}
	lw.mu.Unlock()
}

// editText 读回日志控件里的文本（RichEdit 也支持 GetWindowText）。
func editText(t *testing.T) string {
	t.Helper()
	edit := lw.editHandle()
	if edit == 0 {
		t.Fatal("日志控件不存在")
	}
	buf := make([]uint16, 16384)
	n, _, _ := procGetWindowTextW.Call(uintptr(edit), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// emitWarnLog 往窗口日志里写一条警告级别的记录（走真实的显示层）。
func emitWarnLog(t *testing.T, msg string) {
	t.Helper()
	h := newWindowHandler(lw, slog.LevelDebug)
	r := slog.NewRecord(time.Now(), slog.LevelWarn, msg, 0)
	r.AddAttrs(slog.String("err", "Access is denied."))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
}
