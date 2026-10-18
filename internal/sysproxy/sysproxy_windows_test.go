//go:build windows

package sysproxy

import (
	"strings"
	"testing"
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
