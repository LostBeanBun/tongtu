//go:build windows

package sysproxy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMergeBypassKeepsUserEntries 接管时必须保留用户原有的绕过项，同时补齐必需项。
func TestMergeBypassKeepsUserEntries(t *testing.T) {
	existing := "*.corp.local;10.*"
	got := mergeBypass(&existing)
	for _, want := range append([]string{"*.corp.local", "10.*"}, requiredBypass...) {
		if !strings.Contains(got, want) {
			t.Errorf("合并结果缺少 %q: %q", want, got)
		}
	}
}

// TestMergeBypassDedupes 已存在的必需项不重复写入。
func TestMergeBypassDedupes(t *testing.T) {
	existing := "localhost;intranet"
	got := mergeBypass(&existing)
	if n := strings.Count(got, "localhost"); n != 1 {
		t.Errorf("localhost 应只出现一次，实际 %d 次: %q", n, got)
	}
	if !strings.Contains(got, "intranet") {
		t.Errorf("用户条目丢失: %q", got)
	}
}

// TestMergeBypassAllPresent 原列表已含全部必需项时逐字返回，不做无谓改写。
func TestMergeBypassAllPresent(t *testing.T) {
	existing := "localhost;127.0.0.1;::1;<local>;intranet"
	if got := mergeBypass(&existing); got != existing {
		t.Errorf("已含必需项时不应改动列表: got %q want %q", got, existing)
	}
}

// TestMergeBypassNil 没有原值时使用内置必需项。
func TestMergeBypassNil(t *testing.T) {
	if got := mergeBypass(nil); got != "localhost;127.0.0.1;::1;<local>" {
		t.Errorf("无原值时应用内置列表: %q", got)
	}
}

// TestSplitBypass 忽略空白与空项。
func TestSplitBypass(t *testing.T) {
	got := splitBypass(" a ; ;b;;")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("拆分结果错误: %#v", got)
	}
}

// TestDeadLocalProxy 识别「崩溃遗留的典型形态」：代理开着、指向本机回环、端口没人监听。
//
// 这是状态文件损坏时唯一能安全做的兜底判断——只在端口确实死了的情况下才关代理，
// 避免把用户自己正在用的本地代理（Clash 等）关掉。
func TestDeadLocalProxy(t *testing.T) {
	on, off := uint(1), uint(0)
	dead := deadLoopbackAddr(t)
	live := liveLoopbackAddr(t)

	cases := []struct {
		name string
		v    *Values
		want bool
	}{
		{"指向死掉的本地端口", &Values{ProxyEnable: &on, ProxyServer: &dead}, true},
		{"本地端口有人监听", &Values{ProxyEnable: &on, ProxyServer: &live}, false},
		{"代理没开", &Values{ProxyEnable: &off, ProxyServer: &dead}, false},
		{"指向外部地址", &Values{ProxyEnable: &on, ProxyServer: ptr("example.com:8080")}, false},
		{"多协议写法", &Values{ProxyEnable: &on, ProxyServer: ptr("http=127.0.0.1:1;https=127.0.0.1:2")}, false},
		{"空地址", &Values{ProxyEnable: &on, ProxyServer: ptr("")}, false},
		{"没有地址", &Values{ProxyEnable: &on}, false},
		{"空值", nil, false},
	}
	for _, c := range cases {
		if got := deadLocalProxy(c.v); got != c.want {
			t.Errorf("%s: deadLocalProxy = %v, want %v（地址 %v）", c.name, got, c.want, c.v)
		}
	}
}

// TestStateRoundTripAndAtomic 状态文件必须能原样读回，且不留临时文件。
func TestStateRoundTripAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysproxy.json")
	on, server := uint(1), "127.0.0.1:7890"
	want := stateFile{Saved: Values{ProxyEnable: &on, ProxyServer: &server}, Addr: "127.0.0.1:19180", SavedAt: time.Now()}

	if err := writeState(path, want); err != nil {
		t.Fatalf("写状态文件失败: %v", err)
	}
	got, err := readState(path)
	if err != nil {
		t.Fatalf("读状态文件失败: %v", err)
	}
	if got.Addr != want.Addr || got.Saved.ProxyEnable == nil || *got.Saved.ProxyEnable != 1 ||
		got.Saved.ProxyServer == nil || *got.Saved.ProxyServer != server {
		t.Errorf("读回的内容不一致: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("不应残留临时文件")
	}
}

// TestReadStateCorrupt 半截 JSON 必须被识别成「损坏」，而不是普通的读取错误——
// 调用方要靠它决定走 disableCorruptState 的兜底。
func TestReadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysproxy.json")
	if err := os.WriteFile(path, []byte(`{"saved":{"proxy_enable":1`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(path); !errors.Is(err, ErrStateCorrupt) {
		t.Errorf("半截 JSON 应返回 ErrStateCorrupt，实际: %v", err)
	}
	if _, err := readState(filepath.Join(dir, "none.json")); !os.IsNotExist(err) {
		t.Errorf("文件不存在应是 os.ErrNotExist，实际: %v", err)
	}
}

// deadLoopbackAddr 返回一个「刚才还监听过、现在已经关掉」的回环地址。
func deadLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听回环端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// liveLoopbackAddr 返回一个仍在监听的地址（测试结束自动关闭）。
func liveLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听回环端口失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// TestRetargetPlan 代理换口时只改「仍由我们持有」的注册表值，
// 并保留 ProxyEnable / ProxyOverride 与接管前原值。
func TestRetargetPlan(t *testing.T) {
	on := uint(1)
	savedServer := "127.0.0.1:1080"
	st := &stateFile{
		Saved: Values{ProxyEnable: &on, ProxyServer: &savedServer},
		Addr:  "127.0.0.1:19180",
	}
	live := &Values{
		ProxyEnable:   &on,
		ProxyServer:   ptr("127.0.0.1:19180"),
		ProxyOverride: ptr("localhost;<local>"),
	}

	next, changed := retargetPlan(st, live, "127.0.0.1:19181")
	if !changed || next == nil {
		t.Fatal("仍由我们持有时应生成换口计划")
	}
	if next.ProxyServer == nil || *next.ProxyServer != "127.0.0.1:19181" {
		t.Fatalf("ProxyServer 未更新: %+v", next.ProxyServer)
	}
	if next.ProxyEnable == nil || *next.ProxyEnable != 1 {
		t.Fatal("ProxyEnable 不应被改掉")
	}
	if next.ProxyOverride == nil || *next.ProxyOverride != "localhost;<local>" {
		t.Fatal("ProxyOverride 不应被改掉")
	}
	if *st.Saved.ProxyServer != savedServer {
		t.Fatal("接管前原值不应被换口改写")
	}

	if _, changed := retargetPlan(st, live, "127.0.0.1:19180"); changed {
		t.Error("地址没变时不应生成换口计划")
	}

	theirs := &Values{ProxyEnable: &on, ProxyServer: ptr("127.0.0.1:7890")}
	if _, changed := retargetPlan(st, theirs, "127.0.0.1:19181"); changed {
		t.Error("系统代理已被其他程序接管时不应改注册表")
	}

	if _, changed := retargetPlan(nil, live, "127.0.0.1:19181"); changed {
		t.Error("没有状态文件时不应改注册表")
	}
	if _, changed := retargetPlan(st, nil, "127.0.0.1:19181"); changed {
		t.Error("读不到当前注册表值时不应改注册表")
	}
	if _, changed := retargetPlan(st, live, ""); changed {
		t.Error("空地址不应生成换口计划")
	}
}

// TestHeldIn 只有「代理开着且地址就是接管地址」才算仍由通途持有。
func TestHeldIn(t *testing.T) {
	on, off := uint(1), uint(0)
	ours, theirs := "127.0.0.1:19180", "127.0.0.1:7890"
	cases := []struct {
		name string
		v    *Values
		want bool
	}{
		{"仍由通途持有", &Values{ProxyEnable: &on, ProxyServer: &ours}, true},
		{"别的程序接管", &Values{ProxyEnable: &on, ProxyServer: &theirs}, false},
		{"用户关掉了代理", &Values{ProxyEnable: &off, ProxyServer: &ours}, false},
		{"地址被删", &Values{ProxyEnable: &on}, false},
		{"开关被删", &Values{ProxyServer: &ours}, false},
		{"空值", nil, false},
	}
	for _, c := range cases {
		if got := heldIn(c.v, ours); got != c.want {
			t.Errorf("%s: heldIn = %v, want %v", c.name, got, c.want)
		}
	}
	if heldIn(&Values{ProxyEnable: &on, ProxyServer: &ours}, "") {
		t.Error("接管地址为空时不应算作持有")
	}
}

// TestPlanDisableOwnership 【高2】Disable 的持有校验必须绑在「本轮接管地址」上，
// 且状态文件的去留按归属决定——多实例先后接管的还原链全靠这两条不出错。
func TestPlanDisableOwnership(t *testing.T) {
	off := uint(0)
	origA, addrA, addrB, clash := "127.0.0.1:1080", "127.0.0.1:19180", "127.0.0.1:19181", "127.0.0.1:7890"
	pointsTo := func(addr string, enable uint) *Values {
		return &Values{ProxyEnable: &enable, ProxyServer: ptr(addr)}
	}
	savedA := &Values{ProxyEnable: &off, ProxyServer: ptr(origA)} // A 接管前的原值
	stA := func() *stateFile { return &stateFile{Saved: *savedA, Addr: addrA} }

	cases := []struct {
		name       string
		st         *stateFile
		saved      *Values
		heldAddr   string
		live       *Values
		liveOK     bool
		want       Outcome
		wantNext   bool // 是否执行「按原值还原」
		wantRemove bool // 是否删除状态文件
	}{
		{"单实例正常退出", stA(), savedA, addrA, pointsTo(addrA, 1), true, Restored, true, true},

		// 多实例链（核心）：B 接管后文件被覆盖成「原值=addrA、地址=addrB」。
		// A 先退出：注册表已是 addrB ≠ A 的本轮地址 → 不动注册表、**不删文件**
		//（文件里存着 B 退出时要用的原值，删了链就断了）。
		{"A 先退出：文件已被 B 覆盖", &stateFile{Saved: Values{ProxyServer: ptr(addrA)}, Addr: addrB},
			savedA, addrA, pointsTo(addrB, 1), true, NotHeld, false, false},

		// B 后退出（其文件已被 A 的旧实现删掉的场景）：没有文件也不能跳过校验——
		// 注册表指向的若已不是 B 的本轮地址，绝不能把死地址当原值还原回去。
		{"B 后退出：无文件也不跳过校验", nil,
			&Values{ProxyServer: ptr(addrA)}, addrB, pointsTo(clash, 1), true, NotHeld, false, false},

		{"用户手动关掉了代理", stA(), savedA, addrA, pointsTo(addrA, off), true, NotHeld, false, true},
		{"被其他程序接管", stA(), savedA, addrA, pointsTo(clash, 1), true, NotHeld, false, true},
		{"读不到注册表时按原值还原", stA(), savedA, addrA, nil, false, Restored, true, true},
		{"崩溃恢复：文件与注册表一致", stA(), &stA().Saved, addrA, pointsTo(addrA, 1), true, Restored, true, true},
		{"崩溃恢复：已被其他程序接管", stA(), &stA().Saved, addrA, pointsTo(clash, 1), true, NotHeld, false, true},
		{"校验地址为空：拒绝一切还原", stA(), savedA, "", pointsTo(addrA, 1), true, NotHeld, false, false},
	}
	for _, c := range cases {
		got := planDisable(c.st, c.saved, c.heldAddr, c.live, c.liveOK)
		if got.outcome != c.want {
			t.Errorf("%s: outcome = %v, want %v", c.name, got.outcome, c.want)
		}
		if (got.next != nil) != c.wantNext {
			t.Errorf("%s: 执行还原 = %v, want %v", c.name, got.next != nil, c.wantNext)
		}
		if c.wantNext && got.next != c.saved {
			t.Errorf("%s: 还原值应是调用方本轮的原值", c.name)
		}
		if got.removeState != c.wantRemove {
			t.Errorf("%s: 删除状态文件 = %v, want %v", c.name, got.removeState, c.wantRemove)
		}
	}
}

// TestPeerHoldsSystemProxy 「同袍实例正持有系统代理」的三条件缺一不可：
// 少任何一个都会导致误抢（还原链被覆盖）或误拒（用户永远接管不上）。
func TestPeerHoldsSystemProxy(t *testing.T) {
	on := uint(1)
	addrA, addrB := "127.0.0.1:19180", "127.0.0.1:19181"
	pointsTo := func(addr string) *Values {
		return &Values{ProxyEnable: &on, ProxyServer: ptr(addr)}
	}
	peerSt := &stateFile{Addr: addrB}
	alwaysListen := func(string) bool { return true }
	neverListen := func(string) bool { return false }

	if !peerHoldsSystemProxy(pointsTo(addrB), peerSt, addrA, alwaysListen) {
		t.Error("注册表指向同袍地址且它仍在监听：应判定为同袍持有（本轮不接管）")
	}
	if peerHoldsSystemProxy(pointsTo(addrB), nil, addrA, alwaysListen) {
		t.Error("没有状态文件时不能凭空判定同袍持有")
	}
	if peerHoldsSystemProxy(pointsTo(addrB), peerSt, addrB, alwaysListen) {
		t.Error("记录地址就是本轮地址：不是同袍（自己还没建立接管）")
	}
	if peerHoldsSystemProxy(pointsTo(addrA), peerSt, addrA, alwaysListen) {
		t.Error("注册表不指向记录地址：那轮接管已不生效，不算有人持有")
	}
	if peerHoldsSystemProxy(pointsTo(addrB), peerSt, addrA, neverListen) {
		t.Error("记录地址已无人监听：是遗留状态而非活的同袍，应按正常接管处理")
	}
}
