package app

// 系统代理「还原失败」与「暂停中重复停止」的状态一致性回归：
//
//   - Disable 出错时现实仍是「持有」，状态必须原样保留——报成已关会让所有界面
//     撒谎，清掉 sysSaved 更是把原值永久弄丢（下一次 Enable 会把自己的地址存成
//     "原值"，还原链从此被毒化）。
//   - 暂停中再次 StopAll 必须幂等：从「已还原的状态」重算会得出没有暂停项，
//     把暂停记录整个抹掉（状态词从「已暂停」跳回「运行中」）。
//
// 注册表写失败在真机上难以稳定复现，这里通过 sysHeld / sysDisable 等注入点
// （见 app.go 顶部）模拟，全程不碰测试机的真实注册表。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"tongtu/internal/config"
	"tongtu/internal/rules"
	"tongtu/internal/sysproxy"
)

// fakeSys 注入系统代理操作的行为：held 决定「当前是否持有」，
// disableErr / enableErr 非 nil 时模拟注册表写失败，
// live / liveErr 决定「当前注册表原值」的读取结果（给 ProxyNote 归因用）。
type fakeSys struct {
	held         bool
	disableErr   error
	enableErr    error
	disableCalls int
	live         *sysproxy.Values
	liveErr      error
}

// install 替换包级注入点并在测试结束时还原（必须在 startTestApp 之前调用，
// 这样 Start / Shutdown 的生命周期全程都在假实现的保护下）。
func (f *fakeSys) install(t *testing.T) {
	t.Helper()
	oldHeld, oldEnable, oldDisable, oldRetarget, oldLive := sysHeld, sysEnable, sysDisable, sysRetarget, sysLive
	sysHeld = func(string) (bool, error) { return f.held, nil }
	sysLive = func() (*sysproxy.Values, error) { return f.live, f.liveErr }
	sysEnable = func(string) (*sysproxy.Values, error) {
		if f.enableErr != nil {
			return nil, f.enableErr
		}
		f.held = true
		return &sysproxy.Values{}, nil
	}
	sysDisable = func(*sysproxy.Values, string) (sysproxy.Outcome, error) {
		f.disableCalls++
		if f.disableErr != nil {
			return sysproxy.NotHeld, f.disableErr
		}
		f.held = false
		return sysproxy.Restored, nil
	}
	sysRetarget = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() {
		sysHeld, sysEnable, sysDisable, sysRetarget, sysLive = oldHeld, oldEnable, oldDisable, oldRetarget, oldLive
	})
}

// startTestApp 起一个「不接管系统代理」的实例（配置写到临时文件）。
// 调用方自己在锁内摆好 sysOn / sysSaved 等内部状态。
func startTestApp(t *testing.T) *App {
	t.Helper()
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.MITM.Enabled = false, false
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{
		Config: cfg, Rules: rs, Log: quiet,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a
}

// markHeldForTest 直接摆内部状态：模拟「本轮已接管系统代理」。
func markHeldForTest(a *App) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sysOn = true
	a.sysSaved = &sysproxy.Values{}
}

// TestSetModeDisableFailureKeepsHeld 【高3】还原失败时不许清状态：
// 界面继续报「持有」，原值保留（否则毒化链从这里开始）。
func TestSetModeDisableFailureKeepsHeld(t *testing.T) {
	fake := &fakeSys{held: true, disableErr: errors.New("写注册表失败")}
	fake.install(t)
	a := startTestApp(t)
	markHeldForTest(a)

	if err := a.SetMode(false); err == nil {
		t.Fatal("还原失败必须把错误返回给调用方，不能静默报成功")
	}
	if fake.disableCalls != 1 {
		t.Fatalf("应恰好调用一次 Disable，实际 %d 次", fake.disableCalls)
	}
	st := a.Status()
	if !st.SystemProxy {
		t.Fatal("还原失败时注册表仍指向通途，Status 不能报「已关闭」（界面会撒谎）")
	}
	if st.ProxyNote != "" {
		t.Errorf("仍持有不该带「被接管」之类的原因备注: %q", st.ProxyNote)
	}
	a.mu.Lock()
	savedKept := a.sysSaved != nil
	a.mu.Unlock()
	if !savedKept {
		t.Fatal("还原失败时原值不能清空：清了就再也还原不回去（下一次 Enable 会把自己的地址当原值）")
	}
}

// TestStopAllDisableFailureKeepsHeld 【高3】暂停路径同款：
// Disable 失败 → 仍报告持有，且这一项没暂停成就不记进暂停记录。
func TestStopAllDisableFailureKeepsHeld(t *testing.T) {
	fake := &fakeSys{held: true, disableErr: errors.New("写注册表失败")}
	fake.install(t)
	a := startTestApp(t)
	markHeldForTest(a)

	if err := a.StopAll(); err == nil {
		t.Fatal("还原失败必须返回错误")
	}
	if !a.Status().SystemProxy {
		t.Fatal("还原失败时仍持有，不能报成已关闭")
	}
	if a.Paused() {
		t.Fatal("系统代理没暂停成功，不该记成「已暂停」（否则恢复流程会误判）")
	}
}

// TestStopAllDisableSuccessPauses 正常路径不受影响：还原成功才进入暂停态、
// 状态报关闭——确认上面的修复没有把成功分支一起拦掉。
func TestStopAllDisableSuccessPauses(t *testing.T) {
	fake := &fakeSys{held: true}
	fake.install(t)
	a := startTestApp(t)
	markHeldForTest(a)

	if err := a.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if !a.Paused() {
		t.Fatal("还原成功应进入暂停态")
	}
	if a.Status().SystemProxy {
		t.Fatal("还原成功后不应再报持有")
	}
	a.mu.Lock()
	sysOn := a.sysOn
	a.mu.Unlock()
	if sysOn {
		t.Fatal("还原成功后 sysOn 应清掉")
	}
}

// TestStopAllWhilePausedKeepsRecord 【低2】暂停中再 StopAll 必须幂等：
// 暂停后 sysOn 已被清掉，重算会得出「没有暂停项」并把暂停记录抹掉。
func TestStopAllWhilePausedKeepsRecord(t *testing.T) {
	a := startTestApp(t)

	// 摆一个「系统代理被暂停」的记录（进程仍在跑，对外影响已撤回）
	a.mu.Lock()
	a.paused = &pauseState{sys: true}
	a.mu.Unlock()

	if err := a.StopAll(); err != nil {
		t.Fatalf("暂停中再次 StopAll 应幂等返回 nil，实际: %v", err)
	}
	if !a.Paused() {
		t.Fatal("暂停中再次 StopAll 把暂停记录抹掉了（状态词会从「已暂停」跳回「运行中」）")
	}
	if !a.Status().Paused {
		t.Fatal("Status 应继续报告暂停状态")
	}
}

// TestSetModeRecordsWantProxy 【中4】运行时切换必须更新「意图」：
// 托盘 / 窗口的开关显示的是实际持有，点击后意图要跟着走——否则关掉之后
// 意图还停在「要开」，状态条会一直报「部分生效」。接管失败时意图同样要记下
// （界面如实报降级），这正是 Status.WantSystemProxy 存在的意义。
func TestSetModeRecordsWantProxy(t *testing.T) {
	fake := &fakeSys{} // 接管成功
	fake.install(t)
	a := startTestApp(t)

	if a.Status().WantSystemProxy {
		t.Fatal("启动时没接管（等价 --no-sysproxy），初始意图应为 false")
	}
	if err := a.SetMode(true); err != nil {
		t.Fatalf("SetMode(true): %v", err)
	}
	st := a.Status()
	if !st.WantSystemProxy {
		t.Fatal("托盘点了开之后意图应为 true")
	}
	if !st.SystemProxy {
		t.Fatal("假实现接管成功后应报实际持有")
	}
	if err := a.SetMode(false); err != nil {
		t.Fatalf("SetMode(false): %v", err)
	}
	if a.Status().WantSystemProxy {
		t.Fatal("点了关之后意图应为 false（否则状态条永远「部分生效」）")
	}
}

// TestSetModeWantRecordedOnFailure 接管失败也要记下意图：
// 用户表达了「要开」，界面就该如实报「部分生效」，而不是装作没这回事。
func TestSetModeWantRecordedOnFailure(t *testing.T) {
	fake := &fakeSys{enableErr: errors.New("写注册表失败")}
	fake.install(t)
	a := startTestApp(t)

	if err := a.SetMode(true); err == nil {
		t.Fatal("接管失败必须把错误返回给调用方")
	}
	st := a.Status()
	if !st.WantSystemProxy {
		t.Fatal("接管失败也要记录「要开」的意图")
	}
	if st.SystemProxy {
		t.Fatal("接管失败时不能报实际持有")
	}
}

// TestProxyLossNote 【低4】接管没生效的归因必须照注册表实况说：
// 代理还开着才是「被别的程序接管」，代理本身关着只能说「已被关闭」，
// 读不出状态就老实报「状态读取失败」——一律写「被其他程序接管」是泼错脏水。
func TestProxyLossNote(t *testing.T) {
	enabled := uint(1)
	disabled := uint(0)
	other := "127.0.0.1:7890"
	cases := []struct {
		name string
		v    *sysproxy.Values
		err  error
		want string
	}{
		{name: "读取失败", err: errors.New("打开注册表失败"), want: "状态读取失败"},
		{name: "代理开着但地址不是我们", v: &sysproxy.Values{ProxyEnable: &enabled, ProxyServer: &other}, want: "已被其他程序接管"},
		{name: "代理被关掉", v: &sysproxy.Values{ProxyEnable: &disabled}, want: "已被关闭"},
		{name: "代理值整个不存在", v: &sysproxy.Values{}, want: "已被关闭"},
		{name: "nil 值", v: nil, want: "已被关闭"},
	}
	for _, c := range cases {
		if got := proxyLossNote(c.v, c.err); got != c.want {
			t.Errorf("%s: 应报 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// TestStatusProxyNoteAttribution 【低4】Status 级：接管过但没持有住时，
// ProxyNote 必须按当前注册表实况归因，而不是一律「已被其他程序接管」。
func TestStatusProxyNoteAttribution(t *testing.T) {
	enabled := uint(1)
	disabled := uint(0)
	other := "127.0.0.1:7890"

	cases := []struct {
		name  string
		live  *sysproxy.Values
		liveE error
		want  string
	}{
		{name: "被别的程序顶掉", live: &sysproxy.Values{ProxyEnable: &enabled, ProxyServer: &other}, want: "已被其他程序接管"},
		{name: "代理被关掉", live: &sysproxy.Values{ProxyEnable: &disabled}, want: "已被关闭"},
		{name: "读取失败", liveE: errors.New("打开注册表失败"), want: "状态读取失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeSys{held: false, live: c.live, liveErr: c.liveE}
			fake.install(t)
			a := startTestApp(t)
			markHeldForTest(a) // sysOn=true：接管过
			// fake.held=false：注册表里已经不是我们了

			st := a.Status()
			if st.SystemProxy {
				t.Fatal("没持有住时不能报实际生效")
			}
			if st.ProxyNote != c.want {
				t.Errorf("ProxyNote 应为 %q，实际 %q", c.want, st.ProxyNote)
			}
		})
	}
}

// TestHealthSystemProxyNoteAttribution 【低4】health 级同款归因：
// 降级原因（reason）要如实说明是被顶掉还是被关掉。
func TestHealthSystemProxyNoteAttribution(t *testing.T) {
	enabled := uint(1)
	disabled := uint(0)
	other := "127.0.0.1:7890"

	cases := []struct {
		name string
		live *sysproxy.Values
		want string
	}{
		{name: "被别的程序顶掉", live: &sysproxy.Values{ProxyEnable: &enabled, ProxyServer: &other}, want: "系统代理已被其他程序接管"},
		{name: "代理被关掉", live: &sysproxy.Values{ProxyEnable: &disabled}, want: "系统代理已被关闭"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeSys{held: false, live: c.live}
			fake.install(t)
			a := startTestApp(t)
			markHeldForTest(a)

			a.mu.Lock()
			issues := a.healthIssuesLocked()
			a.mu.Unlock()
			for _, iss := range issues {
				if iss.kind == issueSystemProxy {
					if iss.reason != c.want {
						t.Errorf("降级原因应为 %q，实际 %q", c.want, iss.reason)
					}
					return
				}
			}
			t.Fatalf("应报出 issueSystemProxy，实际 %+v", issues)
		})
	}
}
