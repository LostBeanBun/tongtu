//go:build windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"tongtu/internal/selfcheck"
)

// 本文件单测「交互层」：把鼠标消息真的发给窗口过程，断言动作被正确触发。
//
// 为什么值得这么测：命中判定、动作分发、反馈（轻量提示 / 日志 / 过滤）串在一起才是
// 用户看到的「点得动」，而它们各自都很容易在改动里悄悄错位（几何改了、id 写错了、
// 失败路径忘了给反馈）。这里用假的 windowActions 记录调用，不碰真实系统。

// fakeActions 记录界面触发过的动作。
//
// 开关切换现在跑在后台 goroutine 上（见 toggleMode），所以这里的计数都要加锁，
// 并且可以挂一个 gate 把切换卡住，用来观察「正在切换」时的界面表现。
type fakeActions struct {
	mu           sync.Mutex
	gate         chan struct{} // 非 nil 时 ToggleMode 会等它被关闭
	toggleCalls  []fakeToggle
	toggleErr    error
	refreshHits  int
	quitHits     int
	configHits   int
	selfTest     []selfcheck.Row // SelfTest 的返回值
	selfTestHit  int
	selfTestGate chan struct{} // 非 nil 时 SelfTest 会等它被关闭（模拟「体检中」）
	certMissing  bool          // true 表示「证书还没装」（默认已装：否则会弹模态框把测试卡住）
	certHits     int
	todoItems    []uiTodoItem
	todoRuns     []string
}

type fakeToggle struct {
	ID string
	On bool
}

func (f *fakeActions) actions() windowActions {
	return windowActions{
		ToggleMode: func(id string, on bool) error {
			f.mu.Lock()
			f.toggleCalls = append(f.toggleCalls, fakeToggle{id, on})
			gate, err := f.gate, f.toggleErr
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			return err
		},
		SetServices: func([]string) error { return nil },
		Services:    func() []uiService { return nil },
		OpenConfig: func() error {
			f.mu.Lock()
			f.configHits++
			f.mu.Unlock()
			return nil
		},
		Quit: func() {
			f.mu.Lock()
			f.quitHits++
			f.mu.Unlock()
		},
		Refresh: func() {
			f.mu.Lock()
			f.refreshHits++
			f.mu.Unlock()
		},
		SelfTest: func(context.Context) []selfcheck.Row {
			f.mu.Lock()
			f.selfTestHit++
			gate, rows := f.selfTestGate, f.selfTest
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			return rows
		},
		CertReady: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.certHits++
			return !f.certMissing
		},
		InstallCert: func() error { return nil },
		Todos:       func() []uiTodoItem { return f.todoItems },
		RunTodo: func(id string) error {
			f.mu.Lock()
			f.todoRuns = append(f.todoRuns, id)
			f.mu.Unlock()
			return nil
		},
	}
}

func (f *fakeActions) calls() []fakeToggle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeToggle(nil), f.toggleCalls...)
}

func (f *fakeActions) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshHits
}

func (f *fakeActions) quits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quitHits
}

func (f *fakeActions) configs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configHits
}

func (f *fakeActions) selfTestCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfTestHit
}

// useFakeActions 注入假动作，并在测试结束时清掉（避免影响其它测试）。
func useFakeActions(t *testing.T, f *fakeActions) {
	t.Helper()
	setWindowActionsHook(f.actions)
	t.Cleanup(func() {
		setWindowActionsHook(nil)
		// 等后台切换收尾，别把「切换中」的状态留给后面的测试
		waitFor(t, "后台切换结束", func() bool {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			return lw.pendingMode == ""
		})
	})
}

// waitFor 轮询等待条件成立（后台动作是异步的），最多等 5 秒。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// toastText 取当前轻量反馈的文案。
func toastText() string { return lw.activeToast().Text }

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
	waitFor(t, "发出开启 hosts 的请求", func() bool { return len(f.calls()) == 1 })
	if c := f.calls(); c[0].ID != uiIDHosts || !c[0].On {
		t.Fatalf("点 Hosts 胶囊应请求开启 hosts，实际 %+v", c)
	}
	// 切换跑在后台：窗口线程先给「正在切换」，做完再换成结果文案
	waitFor(t, "切换完成的反馈", func() bool { return toastText() == "Hosts已开启" })
	if got := lw.activeToast(); got.Style != styleOK {
		t.Errorf("成功反馈应带成功样式，实际 %+v", got)
	}
	waitFor(t, "操作成功后回读一次状态", func() bool { return f.refreshes() == 1 })

	// 系统代理当前是开的：点一下应该请求「关闭」
	clickRegion(t, uiIDSys)
	waitFor(t, "发出关闭系统代理的请求", func() bool { return len(f.calls()) == 2 })
	if c := f.calls(); c[1].ID != uiIDSys || c[1].On {
		t.Fatalf("点系统代理胶囊应请求关闭，实际 %+v", c)
	}
	waitFor(t, "关闭后的反馈文案", func() bool { return toastText() == "系统代理已关闭" })
}

// TestToggleWhilePendingIsIgnored：切换还没做完时再点开关，不发第二次请求
// （状态尚未回读，连着点只会得到与期望相反的结果），只更新一句提示。
func TestToggleWhilePendingIsIgnored(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, true, true)
	f := &fakeActions{gate: make(chan struct{})}
	useFakeActions(t, f)
	noToast(t)

	clickRegion(t, uiIDHosts)
	waitFor(t, "发起第一次切换", func() bool { return len(f.calls()) == 1 })
	// 后台被 gate 卡住：此刻界面必须仍然有反馈，而且不能卡住窗口线程
	if got := toastText(); got != "Hosts正在切换…" {
		t.Errorf("切换期间应显示进行中的提示，实际 %q", got)
	}

	clickRegion(t, uiIDHosts)
	if n := len(f.calls()); n != 1 {
		t.Errorf("切换期间再点不该发起新请求，实际 %d 次", n)
	}
	if got := toastText(); !strings.Contains(got, "请稍候") {
		t.Errorf("切换期间再点应提示稍候，实际 %q", got)
	}

	close(f.gate)
	waitFor(t, "切换完成", func() bool { return toastText() == "Hosts已开启" })
	waitFor(t, "pendingMode 清空", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return lw.pendingMode == ""
	})
	// 完成之后再点：又能正常发请求了
	clickRegion(t, uiIDHosts)
	waitFor(t, "完成后可以再次切换", func() bool { return len(f.calls()) == 2 })
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
	waitFor(t, "发起切换", func() bool { return len(f.calls()) == 1 })
	if c := f.calls(); c[0].ID != uiIDMITM || !c[0].On {
		t.Fatalf("只应请求开启反代这一项，实际 %+v", c)
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
	if n := f.quits(); n != 0 {
		t.Errorf("在别处松开不该退出，实际退出 %d 次", n)
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
	if len(f.calls()) != 0 || f.quits() != 0 || f.configs() != 0 {
		t.Errorf("点空白处不该触发动作：请求 %v，退出 %d 次，打开配置 %d 次",
			f.calls(), f.quits(), f.configs())
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
	waitFor(t, "失败反馈", func() bool { return toastText() == "需要管理员权限" })
	if got := lw.activeToast(); got.Style != styleWarn {
		t.Errorf("失败反馈应带警告样式，实际 %+v", got)
	}
	if n := f.refreshes(); n != 0 {
		t.Errorf("失败时不该回读状态（本来就没变），实际 %d 次", n)
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
	if n := f.configs(); n != 1 {
		t.Errorf("点「打开配置」应调用一次 OpenConfig，实际 %d", n)
	}
	clickRegion(t, uiIDQuit)
	if n := f.quits(); n != 1 {
		t.Errorf("点「退出」应调用一次 Quit，实际 %d", n)
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

func TestClearLogMenuEntryClearsEdit(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	useFakeActions(t, &fakeActions{})

	_, _ = lw.Write([]byte("清空之前的一行日志\n"))
	if got := editText(t); !strings.Contains(got, "清空之前的一行日志") {
		t.Fatalf("准备条件不成立：日志控件里没有刚写入的行：%q", got)
	}
	// 「清空窗口日志」现在是「日志」菜单里的一项：菜单本身是模态的（会阻塞测试），
	// 因此这里直接分发菜单项的 id——这正是选中菜单项后走的那一步。
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	lw.invokeAction(hwnd, uiIDClear)
	if got := editText(t); got != "" {
		t.Errorf("清空日志后日志控件应为空，实际 %q", got)
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

// TestServicesMenuPointIsScreenCoords：「服务组」菜单必须锚在按钮左下角，而且是**屏幕**坐标。
//
// TrackPopupMenu 只认屏幕坐标，而几何层里所有矩形都是客户区坐标：这里不弹菜单（那会阻塞），
// 只验证换算对不对——漏了换算的话菜单会跑到窗口左上角附近的错误位置。
func TestServicesMenuPointIsScreenCoords(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()

	r, ok := lw.regionAtID(uiIDServices)
	if !ok {
		t.Fatal("当前几何里没有「服务组」按钮")
	}
	pt := lw.servicesMenuPoint()

	// 换算回客户区后，应当是按钮的左下角
	back := pt
	procScreenToClient.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&back)))
	if back.X != r.Rect.Left || back.Y != r.Rect.Bottom {
		t.Errorf("菜单位置换算回客户区应是按钮左下角 (%d,%d)，实际 (%d,%d)",
			r.Rect.Left, r.Rect.Bottom, back.X, back.Y)
	}

	// 而且必须落在窗口的屏幕矩形里（用客户区坐标当屏幕坐标用就会跑到框外）
	var wr rectT
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wr)))
	if pt.X < wr.Left || pt.X > wr.Right || pt.Y < wr.Top || pt.Y > wr.Bottom {
		t.Errorf("菜单位置 (%d,%d) 不在窗口的屏幕矩形 (%d,%d)-(%d,%d) 内",
			pt.X, pt.Y, wr.Left, wr.Top, wr.Right, wr.Bottom)
	}
}

// TestSelfTestWritesResultsToCard：点「自检」后，结果要写进日志卡片并给一行反馈。
//
// 自检本身跑在后台（十来个域名串行访问），因此这里用假动作秒回结果，
// 专测「点了之后界面该发生什么」：按钮进入进行中、结果成块、结论回填到状态条。
func TestSelfTestWritesResultsToCard(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, true)
	f := &fakeActions{selfTest: []selfcheck.Row{
		{Host: "github.com", DNSOK: true, ProxyOK: true, ProxyTested: true, Verdict: selfcheck.VerdictOK},
		{Host: "raw.githubusercontent.com", DNSOK: true, DirectOK: true, ProxyTested: true,
			Verdict: selfcheck.VerdictDirectOnly},
	}}
	useFakeActions(t, f)
	lw.clearLog()

	clickRegion(t, uiIDSelfTest)
	waitFor(t, "自检结果写入日志卡片", func() bool {
		return strings.Contains(lw.recentText(0), "自检结果")
	})
	waitFor(t, "状态条给出结论", func() bool {
		return strings.Contains(toastText(), "仅直连")
	})
	waitFor(t, "自检收尾（按钮回到常态）", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.selfTesting
	})

	text := lw.recentText(0)
	if !strings.Contains(text, "github.com") {
		t.Errorf("结果里应有被体检的域名，实际：%q", text)
	}
	if !strings.Contains(text, "仅直连") {
		t.Errorf("没走加速的域名要说明「仅直连」，实际：%q", text)
	}
	if !strings.Contains(text, "合计") {
		t.Errorf("结果末尾应有合计行，实际：%q", text)
	}
	if got := toastText(); !strings.Contains(got, "仅直连") {
		t.Errorf("状态条应反馈「有几个没走加速」，实际 %q", got)
	}
	if n := f.selfTestCalls(); n != 1 {
		t.Errorf("应只跑一次自检，实际 %d 次", n)
	}
}

// TestSelfTestIgnoredWhileRunning：自检进行中重复点只提示，不会再起一次。
func TestSelfTestIgnoredWhileRunning(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, true)
	gate := make(chan struct{})
	f := &fakeActions{selfTest: []selfcheck.Row{{Host: "a", Verdict: selfcheck.VerdictOK}}}
	f.mu.Lock()
	f.selfTestGate = gate
	f.mu.Unlock()
	useFakeActions(t, f)
	lw.clearLog()

	clickRegion(t, uiIDSelfTest)
	waitFor(t, "自检开始", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return lw.selfTesting
	})
	clickRegion(t, uiIDSelfTest) // 第二次点击应被忽略
	if got := toastText(); !strings.Contains(got, "正在进行") {
		t.Errorf("重复点击应提示「正在进行」，实际 %q", got)
	}
	close(gate)
	waitFor(t, "自检收尾", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.selfTesting
	})
	if n := f.selfTestCalls(); n != 1 {
		t.Errorf("自检进行中不该再起一次，实际 %d 次", n)
	}
}
