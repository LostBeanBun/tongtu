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

	"tongtu/internal/config"
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
	pauseGate    chan struct{} // 非 nil 时 StopAll / Resume 会等它被关闭（模拟「暂停中」）
	pauseCalls   []string      // 依次记录 "stop" / "resume"
	pauseErr     error
	proxyAddr    string

	// 自定义站点：站点增删与反代切换也都跑在后台 goroutine 上。
	siteGate    chan struct{} // 非 nil 时站点操作会等它被关闭
	sites       []uiSite
	siteAdds    []string
	siteRemoves []string
	siteMITMs   []fakeSiteMITM
	siteErr     error
}

type fakeSiteMITM struct {
	Pattern string
	On      bool
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
		StopAll: func() error { return f.recordPause("stop") },
		PauseFor: func(d time.Duration) error {
			return f.recordPause("for:" + d.String())
		},
		Resume: func() error { return f.recordPause("resume") },
		ProxyAddr: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.proxyAddr
		},
		Sites: func() []uiSite {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]uiSite(nil), f.sites...)
		},
		AddSite: func(raw string) (string, error) {
			f.mu.Lock()
			f.siteAdds = append(f.siteAdds, raw)
			gate, err := f.siteGate, f.siteErr
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			if err != nil {
				return "", err
			}
			// 用与 app 层相同的规范化，让界面测试看到真实行为（粘贴完整网址也能加）。
			p, err := config.NormalizePattern(raw)
			if err != nil {
				return "", err
			}
			f.mu.Lock()
			f.sites = append(f.sites, uiSite{Pattern: p})
			f.mu.Unlock()
			return p, nil
		},
		RemoveSite: func(pattern string) error {
			f.mu.Lock()
			f.siteRemoves = append(f.siteRemoves, pattern)
			gate, err := f.siteGate, f.siteErr
			if err == nil {
				kept := f.sites[:0]
				for _, s := range f.sites {
					if !strings.EqualFold(s.Pattern, pattern) {
						kept = append(kept, s)
					}
				}
				f.sites = kept
			}
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			return err
		},
		SetSiteMITM: func(pattern string, on bool) error {
			f.mu.Lock()
			f.siteMITMs = append(f.siteMITMs, fakeSiteMITM{Pattern: pattern, On: on})
			gate, err := f.siteGate, f.siteErr
			if err == nil {
				for i := range f.sites {
					if strings.EqualFold(f.sites[i].Pattern, pattern) {
						f.sites[i].MITM = on
					}
				}
			}
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			return err
		},
	}
}

// siteCalls 返回站点动作的记录（加锁拷贝）。
func (f *fakeActions) siteCalls() (adds, removes []string, mitms []fakeSiteMITM) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.siteAdds...),
		append([]string(nil), f.siteRemoves...),
		append([]fakeSiteMITM(nil), f.siteMITMs...)
}

// recordPause 记录一次暂停 / 恢复，并在需要时卡住（模拟耗时的接管 / 还原）。
func (f *fakeActions) recordPause(what string) error {
	f.mu.Lock()
	f.pauseCalls = append(f.pauseCalls, what)
	gate, err := f.pauseGate, f.pauseErr
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

// pauses 返回暂停 / 恢复的调用序列。
func (f *fakeActions) pauses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pauseCalls...)
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
		// 等后台动作收尾，别把「切换中」的状态留给后面的测试
		waitFor(t, "后台切换结束", func() bool {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			return lw.pendingMode == "" && !lw.pauseBusy
		})
		// 自检也可能是上一条用例点起来的：它的结果提示会落到下一条用例里
		// （「已暂停加速」被一句迟到的「自检通过」顶掉，就是这么来的）。
		waitFor(t, "自检收尾", func() bool {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			return !lw.selfTesting
		})
		// 站点增删同样在后台跑；面板状态是全局的，必须还原，否则会影响后面的用例。
		waitFor(t, "站点操作收尾", func() bool {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			return !lw.sitesBusy
		})
		lw.mu.Lock()
		lw.sitesOpen, lw.sites = false, nil
		lw.mu.Unlock()
		lw.layout()
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
	// 把它改成「已过期」再送定时器消息：真实的定时器到点就是这个状态。
	lw.toastMsg.Until = time.Now().Add(-time.Millisecond)
	lw.mu.Unlock()
	procSendMessageW.Call(uintptr(hwnd), wmTimer, uiTimerToast, 0)
	if got := lw.activeToast(); got.Text != "" {
		t.Errorf("定时器到点后提示应消失，实际 %+v", got)
	}
}

// TestToastStaleTimerKeepsFreshToast 上一条提示的定时器提前到点，不能把刚发出的新提示吞掉。
//
// 这不是假想：暂停 / 恢复这类操作会先提示「正在暂停加速…」，几百毫秒后再提示结果，
// 两个 SetTimer 用的同一个 id，旧定时器的 WM_TIMER 可能正好落在新提示刚设置之后。
func TestToastStaleTimerKeepsFreshToast(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.toast("第一条提示", styleMuted)
	lw.toast("第二条提示", styleOK) // 立刻替换：旧定时器还挂在队列里
	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	procSendMessageW.Call(uintptr(hwnd), wmTimer, uiTimerToast, 0)
	if got := lw.activeToast(); got.Text != "第二条提示" {
		t.Fatalf("旧定时器不该清掉新提示，实际 %+v", got)
	}
	lw.mu.Lock()
	lw.toastMsg = uiToast{}
	lw.mu.Unlock()
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

// pausedStatus 摆一份「已暂停」的状态条：三个开关都是关的，暂停按钮变成「恢复」。
func pausedStatus(paused bool) {
	setWindowStatus(windowStatus{
		Title:  "通途 Tongtu " + Version,
		State:  "运行中",
		Level:  stateOK,
		Paused: paused,
		Items: []windowStatusItem{
			{Label: "系统代理", On: !paused, Action: uiIDSys},
			{Label: "Hosts", On: false, Action: uiIDHosts},
			{Label: "无SNI反代", On: !paused, Action: uiIDMITM},
		},
		Detail: []string{"服务组 steam"},
	})
}

// waitToast 等一条包含 want 的提示出现。
//
// 失败时把期间出现过的提示按顺序打出来：提示是会被后来的操作顶掉的，
// 只说「超时」根本看不出是没出现、还是被谁盖掉了。
func waitToast(t *testing.T, what, want string) {
	t.Helper()
	var trail []string
	last := ""
	deadline := time.Now().Add(2 * time.Second)
	for {
		txt := toastText()
		if txt != last {
			trail = append(trail, txt)
			last = txt
		}
		if strings.Contains(txt, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：等待提示 %q 超时，期间的提示：%v", what, want, trail)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPauseButtonStopsAndResumes 选「暂停，直到手动恢复」走 StopAll，状态变成已暂停后
// 同一个按钮走 Resume。
//
// 暂停按钮现在先弹时长菜单，而弹出菜单是模态的（TrackPopupMenu 会一直阻塞到用户选择），
// 单测里没法「点菜单项」，因此这里直接调用菜单选中之后的那一步 pauseFor。
func TestPauseButtonStopsAndResumes(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	pausedStatus(false)
	f := &fakeActions{}
	useFakeActions(t, f)
	waitFor(t, "窗口拿到状态", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.status.Paused
	})

	lw.pauseFor(currentWindowActions(), "pause:0")
	waitFor(t, "暂停被调用", func() bool { return len(f.pauses()) == 1 })
	if got := f.pauses()[0]; got != "for:0s" {
		t.Fatalf("应调用 PauseFor(0)（一直暂停），实际 %q", got)
	}
	waitToast(t, "暂停后的反馈", "已暂停加速")
	waitFor(t, "回读状态", func() bool { return f.refreshes() > 0 })

	// 状态回读后按钮变成「恢复」：再点一次走 Resume
	pausedStatus(true)
	waitFor(t, "按钮变成恢复", func() bool {
		r, ok := lw.regionAtID(uiIDPause)
		return ok && r.Label == "恢复"
	})
	clickRegion(t, uiIDPause)
	waitFor(t, "恢复被调用", func() bool { return len(f.pauses()) == 2 })
	if got := f.pauses()[1]; got != "resume" {
		t.Fatalf("应调用 Resume，实际 %q", got)
	}
	waitToast(t, "恢复后的反馈", "已恢复加速")
}

// TestPauseButtonIgnoresRepeatClicks 暂停还在进行时重复点击只提示，不再派发第二次。
func TestPauseButtonIgnoresRepeatClicks(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	pausedStatus(false)
	f := &fakeActions{pauseGate: make(chan struct{})}
	useFakeActions(t, f)
	waitFor(t, "窗口拿到状态", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.status.Paused
	})

	act := currentWindowActions()
	lw.pauseFor(act, "pause:30m")
	waitFor(t, "暂停被调用", func() bool { return len(f.pauses()) == 1 })
	lw.pauseFor(act, "pause:30m")
	waitFor(t, "重复点击的提示", func() bool { return strings.Contains(toastText(), "请稍候") })
	if n := len(f.pauses()); n != 1 {
		t.Fatalf("暂停进行中不该再次派发，实际 %d 次", n)
	}

	close(f.pauseGate)
	waitFor(t, "暂停收尾", func() bool { return strings.Contains(toastText(), "30 分钟后自动恢复") })
}

// TestPauseFailureReported 暂停失败要把原因写出来（并允许重试），而不是静默留在后台。
func TestPauseFailureReported(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	pausedStatus(false)
	f := &fakeActions{pauseErr: errors.New("hosts 被别的程序改写了")}
	useFakeActions(t, f)
	waitFor(t, "窗口拿到状态", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.status.Paused
	})

	lw.pauseFor(currentWindowActions(), "pause:15m")
	waitFor(t, "失败反馈", func() bool { return strings.Contains(toastText(), "hosts 被别的程序改写了") })
	// 失败后不应该卡在「切换中」
	waitFor(t, "允许重试", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.pauseBusy
	})
}

// TestCopyProxyAddrWithoutAddress 代理还没起来时点「复制代理地址」给提示，不写剪贴板。
func TestCopyProxyAddrWithoutAddress(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	f := &fakeActions{proxyAddr: ""}
	useFakeActions(t, f)

	lw.mu.Lock()
	hwnd := lw.hwnd
	lw.mu.Unlock()
	lw.invokeAction(hwnd, uiIDCopyProxy)
	if got := toastText(); !strings.Contains(got, "没有地址可复制") {
		t.Fatalf("应提示没有地址，实际 %q", got)
	}
}

// —— 自定义站点面板 ——

// TestSitesPanelToggle 「站点」按钮打开 / 关闭卡片区的面板，打开时列出已有站点。
func TestSitesPanelToggle(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{sites: []uiSite{{Pattern: "huggingface.co", MITM: true}}}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = false, nil
	lw.mu.Unlock()

	clickRegion(t, uiIDSites)
	waitFor(t, "面板打开并拉到列表", func() bool {
		p := lw.panelSnapshot()
		return p.Open && len(p.Sites) == 1
	})
	if got := lw.panelSnapshot().Sites; len(got) != 1 || got[0].Pattern != "huggingface.co" || !got[0].MITM {
		t.Fatalf("面板应列出已有站点，实际 %+v", got)
	}
	// 打开后：输入框有几何、日志控件让位
	if _, ok := lw.regionAtID(uiIDSiteAdd); !ok {
		t.Error("面板里应有「添加」按钮")
	}
	if _, ok := lw.regionAtID(uiSiteDelID("huggingface.co")); !ok {
		t.Error("面板里应有该站点的「删除」")
	}
	if _, ok := lw.regionAtID(uiSiteMITMID("huggingface.co")); !ok {
		t.Error("面板里应有该站点的「反代」")
	}

	// 再点一次：回到日志
	clickRegion(t, uiIDSites)
	waitFor(t, "面板关闭", func() bool { return !lw.panelSnapshot().Open })
	if _, ok := lw.regionAtID(uiIDSiteAdd); ok {
		t.Error("关闭后不该还有「添加」按钮")
	}
}

// TestSitesAddFromInput 输入域名 → 点「添加」→ 后台添加 → 面板刷新且输入框清空。
func TestSitesAddFromInput(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = false, nil
	lw.mu.Unlock()

	clickRegion(t, uiIDSites)
	waitFor(t, "面板打开", func() bool { return lw.panelSnapshot().Open })
	if lw.siteEditHandle() == 0 {
		t.Skip("站点输入框未创建")
	}
	lw.setSiteInputText("https://My.Site.com/path")

	clickRegion(t, uiIDSiteAdd)
	waitFor(t, "发起添加", func() bool {
		adds, _, _ := f.siteCalls()
		return len(adds) == 1
	})
	if adds, _, _ := f.siteCalls(); adds[0] != "https://My.Site.com/path" {
		t.Errorf("应把原始输入交给 app 层规范化，实际 %q", adds[0])
	}
	waitToast(t, "添加成功的反馈", "已添加 my.site.com")
	waitFor(t, "面板出现新行", func() bool {
		sites := lw.panelSnapshot().Sites
		return len(sites) == 1 && sites[0].Pattern == "my.site.com"
	})
	if _, ok := lw.regionAtID(uiSiteDelID("my.site.com")); !ok {
		t.Error("新加的站点应出现在面板里")
	}
	waitFor(t, "输入框被清空", func() bool { return lw.siteInputText() == "" })
}

// TestSitesAddEmptyInputWarns 输入框为空时不派发添加，只给一句提示。
func TestSitesAddEmptyInputWarns(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, nil
	lw.mu.Unlock()
	lw.setSiteInputText("")

	clickRegion(t, uiIDSiteAdd)
	if adds, _, _ := f.siteCalls(); len(adds) != 0 {
		t.Errorf("空输入不该发起添加，实际 %v", adds)
	}
	if got := toastText(); !strings.Contains(got, "请先输入域名") {
		t.Errorf("应提示先输入域名，实际 %q", got)
	}
}

// TestSitesDuplicateAddWarns 已经在列表里的域名再添加：直接提示，不派发。
func TestSitesDuplicateAddWarns(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{sites: []uiSite{{Pattern: "my.site.com"}}}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, []uiSite{{Pattern: "my.site.com"}}
	lw.mu.Unlock()
	lw.setSiteInputText("MY.SITE.COM")

	clickRegion(t, uiIDSiteAdd)
	if adds, _, _ := f.siteCalls(); len(adds) != 0 {
		t.Errorf("重复添加不该派发，实际 %v", adds)
	}
	if got := toastText(); !strings.Contains(got, "已经在自定义站点里") {
		t.Errorf("应提示已存在，实际 %q", got)
	}
}

// TestSitesRemoveRow 行内「删除」：后台删除并刷新面板。
func TestSitesRemoveRow(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{sites: []uiSite{{Pattern: "my.site.com"}}}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, []uiSite{{Pattern: "my.site.com"}}
	lw.mu.Unlock()
	lw.layout()

	clickRegion(t, uiSiteDelID("my.site.com"))
	waitFor(t, "发起删除", func() bool {
		_, removes, _ := f.siteCalls()
		return len(removes) == 1
	})
	if _, removes, _ := f.siteCalls(); removes[0] != "my.site.com" {
		t.Errorf("应删除该域名，实际 %q", removes[0])
	}
	waitFor(t, "面板里不再有该行", func() bool { return len(lw.panelSnapshot().Sites) == 0 })
	waitToast(t, "删除后的反馈", "已删除 my.site.com")
}

// TestSitesToggleMITM 行内「反代」胶囊：点一下把当前状态取反后派发。
func TestSitesToggleMITM(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{sites: []uiSite{{Pattern: "my.site.com", MITM: false}}}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, []uiSite{{Pattern: "my.site.com", MITM: false}}
	lw.mu.Unlock()
	lw.layout()

	clickRegion(t, uiSiteMITMID("my.site.com"))
	waitFor(t, "发起反代切换", func() bool {
		_, _, mitms := f.siteCalls()
		return len(mitms) == 1
	})
	if _, _, mitms := f.siteCalls(); mitms[0].Pattern != "my.site.com" || !mitms[0].On {
		t.Fatalf("关着的反代应被打开，实际 %+v", mitms[0])
	}
	waitFor(t, "胶囊变成开", func() bool {
		r, ok := lw.regionAtID(uiSiteMITMID("my.site.com"))
		return ok && r.On
	})

	// 再点一次：开着的应被关掉
	clickRegion(t, uiSiteMITMID("my.site.com"))
	waitFor(t, "发起第二次切换", func() bool {
		_, _, mitms := f.siteCalls()
		return len(mitms) == 2
	})
	if _, _, mitms := f.siteCalls(); mitms[1].On {
		t.Errorf("开着的反代应被关闭，实际 %+v", mitms[1])
	}
}

// TestSitesPanelIgnoresRepeatClicks 站点操作进行中重复点击只提示，不再派发。
func TestSitesPanelIgnoresRepeatClicks(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{sites: []uiSite{{Pattern: "my.site.com"}}, siteGate: make(chan struct{})}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, []uiSite{{Pattern: "my.site.com"}}
	lw.mu.Unlock()
	lw.layout()
	lw.setSiteInputText("other.site.com")

	clickRegion(t, uiIDSiteAdd)
	waitFor(t, "发起添加", func() bool {
		adds, _, _ := f.siteCalls()
		return len(adds) == 1
	})
	clickRegion(t, uiIDSiteAdd)
	if got := toastText(); !strings.Contains(got, "请稍候") {
		t.Errorf("进行中重复点击应提示稍候，实际 %q", got)
	}
	if adds, _, _ := f.siteCalls(); len(adds) != 1 {
		t.Errorf("进行中不该再次派发，实际 %d 次", len(adds))
	}

	close(f.siteGate)
	waitFor(t, "站点操作收尾", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.sitesBusy
	})
}

// TestSitesFailureReported 添加失败要把原因说出来，并允许重试。
func TestSitesFailureReported(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	f := &fakeActions{siteErr: errors.New("不是合法的域名")}
	useFakeActions(t, f)
	lw.mu.Lock()
	lw.sitesOpen, lw.sites = true, nil
	lw.mu.Unlock()
	lw.setSiteInputText("bad host")

	clickRegion(t, uiIDSiteAdd)
	waitFor(t, "失败反馈", func() bool { return strings.Contains(toastText(), "不是合法的域名") })
	waitFor(t, "允许重试", func() bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return !lw.sitesBusy
	})
}

// TestContextMenuTargetsLogEdit 右键菜单只应在日志控件上弹出。//
// 回归保护：WM_CONTEXTMENU 的子窗口句柄在 wParam、坐标在 lParam，
// 早先误把 lParam 当子窗口句柄比较，菜单永远弹不出来。
func TestContextMenuTargetsLogEdit(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.mu.Lock()
	edit := lw.edit
	lw.mu.Unlock()
	if edit == 0 {
		t.Skip("日志控件尚未创建")
	}
	if !lw.isLogEdit(uintptr(edit)) {
		t.Error("日志控件上的右键应被接受")
	}
	if lw.isLogEdit(0) {
		t.Error("没有子窗口（0）时不该弹菜单")
	}
	if lw.isLogEdit(uintptr(edit) + 4) {
		t.Error("别的子窗口不该弹菜单")
	}
}
