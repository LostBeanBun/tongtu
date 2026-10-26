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
