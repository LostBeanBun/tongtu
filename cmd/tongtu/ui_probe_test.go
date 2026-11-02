package main

// 诊断面板的「纯逻辑」测试：不碰窗口，只断言行文案、语气与下一步建议。
// 绘制层（ui_probe_windows.go）只负责把这些文案放到矩形里，因此这里覆盖到
// 每一种结论，就等于锁住了面板上会出现的文字。

import (
	"strings"
	"testing"

	"tongtu/internal/selfcheck"
)

func TestUIProbeLinesFixedShape(t *testing.T) {
	var nilRow *selfcheck.Row
	for _, c := range []struct {
		name  string
		row   *selfcheck.Row
		busy  bool
		err   string
		first string
	}{
		{"未诊断", nilRow, false, "", "未诊断"},
		{"诊断中", nilRow, true, "", "诊断中…"},
		{"失败", nilRow, false, "请输入域名", "诊断失败"},
	} {
		lines := uiProbeLines(c.row, c.busy, c.err)
		if len(lines) != len(uiProbeLabels) {
			t.Fatalf("%s：应固定 %d 行，实际 %d", c.name, len(uiProbeLabels), len(lines))
		}
		if lines[0].Value != c.first {
			t.Errorf("%s：第一行应为 %q，实际 %q", c.name, c.first, lines[0].Value)
		}
		for i, line := range lines {
			if line.Label != uiProbeLabels[i] {
				t.Errorf("%s：第 %d 行标签应为 %q，实际 %q", c.name, i, uiProbeLabels[i], line.Label)
			}
			if line.Value == "" {
				t.Errorf("%s：第 %d 行不该留空（没有数据写「—」）", c.name, i)
			}
		}
	}
}

func TestUIProbeLinesOK(t *testing.T) {
	row := selfcheck.Row{
		Host: "github.com", IP: "1.2.3.4 +2", DNSMs: 42, DNSOK: true,
		TCP: "31ms", TCPOK: true,
		Direct: "HTTP 200", DirectOK: true,
		Proxy: "HTTP 200", ProxyOK: true, ProxyTested: true,
		MITM: "✓", Verdict: selfcheck.VerdictOK,
	}
	lines := uiProbeLines(&row, false, "")
	want := map[int]string{
		0: "可用",
		1: "1.2.3.4 +2 · 42ms",
		2: "31ms",
		3: "HTTP 200",
		4: "HTTP 200",
		5: "✓ 命中（本地解密后剥离 SNI）",
	}
	for i, w := range want {
		if lines[i].Value != w {
			t.Errorf("第 %d 行应为 %q，实际 %q", i, w, lines[i].Value)
		}
	}
	if lines[0].Tone != probeToneOK || lines[4].Tone != probeToneOK {
		t.Errorf("可用 / 经代理成功应是成功色：%+v", lines)
	}
	if got := uiProbeAdvice(&row, false, "", false); !strings.Contains(got, "经本地代理返回") {
		t.Errorf("可用时应说明经代理返回了什么，实际 %q", got)
	}
}

func TestUIProbeLinesDirectOnly(t *testing.T) {
	row := selfcheck.Row{
		Host: "github.com", IP: "1.2.3.4", DNSMs: 12, DNSOK: true,
		TCP: "18ms", TCPOK: true,
		Direct: "HTTP 200", DirectOK: true,
		Proxy: "失败: 超时", ProxyTested: true,
		MITM: "–", Verdict: selfcheck.VerdictDirectOnly,
	}
	lines := uiProbeLines(&row, false, "")
	if lines[0].Value != "仅直连" || lines[0].Tone != probeToneWarn {
		t.Errorf("仅直连应是警告色：%+v", lines[0])
	}
	if lines[4].Tone != probeToneWarn {
		t.Errorf("经代理失败应是警告色（站点本身能用）：%+v", lines[4])
	}
	if lines[5].Value != "– 不经过（保持隧道）" {
		t.Errorf("未命中反代应说明保持隧道，实际 %q", lines[5].Value)
	}
	got := uiProbeAdvice(&row, false, "", false)
	if !strings.Contains(got, "能直连但经代理不通") {
		t.Errorf("仅直连时应给出「检查代理」的下一步，实际 %q", got)
	}
	if !strings.Contains(got, "反代") {
		t.Errorf("仅直连时应提到可以为该站打开反代，实际 %q", got)
	}
}

func TestUIProbeLinesDNSFailure(t *testing.T) {
	row := selfcheck.Row{
		Host: "bad.example", Direct: "未测（解析失败）", Proxy: "未测（解析失败）",
		MITM: "未启用", Verdict: selfcheck.VerdictFail,
	}
	lines := uiProbeLines(&row, false, "")
	if lines[1].Value != "失败" || lines[1].Tone != probeToneErr {
		t.Errorf("解析失败应是失败色：%+v", lines[1])
	}
	for _, i := range []int{2} {
		if lines[i].Value != "—" {
			t.Errorf("解析失败时第 %d 行应是「—」，实际 %q", i, lines[i].Value)
		}
	}
	if !strings.Contains(lines[3].Value, "未测") || !strings.Contains(lines[4].Value, "未测") {
		t.Errorf("解析失败时直连 / 经代理应说明「未测」而不是留空：%+v", lines)
	}
	if got := uiProbeAdvice(&row, false, "", false); !strings.Contains(got, "解析失败") {
		t.Errorf("应指出解析失败与检查 DNS 端点，实际 %q", got)
	}
}

func TestUIProbeLinesTCPFailure(t *testing.T) {
	row := selfcheck.Row{
		Host: "blocked.example", IP: "1.2.3.4", DNSOK: true,
		TCP: "不可达", TCPOK: false,
		Direct: "失败: 超时", Proxy: "失败: 超时", ProxyTested: true,
		MITM: "未启用", Verdict: selfcheck.VerdictFail,
	}
	lines := uiProbeLines(&row, false, "")
	if lines[2].Tone != probeToneErr {
		t.Errorf("TCP 不可达应是失败色：%+v", lines[2])
	}
	got := uiProbeAdvice(&row, false, "", false)
	if !strings.Contains(got, "TCP:443 不可达") || !strings.Contains(got, "dns.static_hosts") {
		t.Errorf("TCP 不通时应给出换优选 IP 的下一步，实际 %q", got)
	}
}

func TestUIProbeProxyNotTested(t *testing.T) {
	row := selfcheck.Row{
		Host: "github.com", IP: "1.2.3.4", DNSOK: true,
		TCP: "20ms", TCPOK: true, Direct: "HTTP 200", DirectOK: true,
		MITM: "未启用", Verdict: selfcheck.VerdictOK,
	}
	lines := uiProbeLines(&row, false, "")
	if lines[4].Value != "未测（本地代理未运行）" || lines[4].Tone != probeToneMuted {
		t.Errorf("代理没跑时经代理应是弱化的「未测」：%+v", lines[4])
	}
	got := uiProbeAdvice(&row, false, "", false)
	if !strings.Contains(got, "本地代理未运行") || !strings.Contains(got, "启动通途") {
		t.Errorf("应说明只验证了直连并建议启动通途，实际 %q", got)
	}
}

// TestUIProbePausedAdviceIsHonest 暂停时经代理测试只说明本地代理可用，
// 系统流量并没有经过它——建议文案必须说清楚，否则用户会以为已经在加速。
func TestUIProbePausedAdviceIsHonest(t *testing.T) {
	row := selfcheck.Row{
		Host: "github.com", IP: "1.2.3.4", DNSOK: true,
		TCP: "20ms", TCPOK: true, Direct: "HTTP 200", DirectOK: true,
		Proxy: "HTTP 200", ProxyOK: true, ProxyTested: true,
		MITM: "未启用", Verdict: selfcheck.VerdictOK,
	}
	got := uiProbeAdvice(&row, false, "", true)
	if !strings.Contains(got, "已暂停") || !strings.Contains(got, "系统流量并未经过") {
		t.Errorf("暂停时不能把「经代理通」说成已加速，实际 %q", got)
	}
	// 暂停时如果诊断本身失败，失败原因比「已暂停」更该出现在前面：
	// 不能因为暂停就把「卡在哪」顶掉。
	fail := selfcheck.Row{Host: "bad.example", Verdict: selfcheck.VerdictFail}
	got = uiProbeAdvice(&fail, false, "", true)
	if !strings.Contains(got, "解析失败") {
		t.Errorf("暂停时失败原因仍应保留，实际 %q", got)
	}
	if !strings.Contains(got, "已暂停") {
		t.Errorf("失败建议也应说明当前已暂停，实际 %q", got)
	}
}

func TestUIProbeAdviceOnError(t *testing.T) {
	got := uiProbeAdvice(nil, false, "诊断需要具体域名，不能用通配写法：*.example.com", false)
	if !strings.Contains(got, "具体域名") {
		t.Errorf("诊断失败时建议区应显示原因，实际 %q", got)
	}
	if got := uiProbeAdvice(nil, true, "", false); !strings.Contains(got, "通常 1–10 秒") {
		t.Errorf("诊断中应说明大概要多久，实际 %q", got)
	}
	if got := uiProbeAdvice(nil, false, "", false); !strings.Contains(got, "不会修改任何配置") {
		t.Errorf("未诊断时应说明这个动作是只读的，实际 %q", got)
	}
}

func TestNormalizeProbeHost(t *testing.T) {
	cases := map[string]string{
		"github.com":                       "github.com",
		"  GitHub.COM  ":                   "github.com",
		"https://github.com/user/repo?x=1": "github.com",
		"example.com:8443":                 "example.com",
		"example.com.":                     "example.com",
	}
	for raw, want := range cases {
		got, err := normalizeProbeHost(raw)
		if err != nil {
			t.Errorf("normalizeProbeHost(%q) 出错：%v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeProbeHost(%q) = %q，期望 %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "   ", "*.example.com", "https://*.example.com/x"} {
		if got, err := normalizeProbeHost(raw); err == nil {
			t.Errorf("normalizeProbeHost(%q) 应拒绝，实际得到 %q", raw, got)
		}
	}
}

// TestUIProbeActions 锁住「失败时才给改配置的动作」这条边界：
// 成功时不诱导用户动配置；未诊断 / 诊断中 / 输入错误时只留一个弱化的「复制结果」，
// 按钮位置不跳，点了也会说明原因。
func TestUIProbeActions(t *testing.T) {
	ids := func(acts []uiProbeAction) []string {
		out := make([]string, 0, len(acts))
		for _, a := range acts {
			out = append(out, a.ID)
		}
		return out
	}

	ok := selfcheck.Row{Host: "github.com", Verdict: selfcheck.VerdictOK}
	direct := selfcheck.Row{Host: "blocked.example", Verdict: selfcheck.VerdictDirectOnly}
	fail := selfcheck.Row{Host: "bad.example", Verdict: selfcheck.VerdictFail}

	for _, c := range []struct {
		name string
		row  *selfcheck.Row
		busy bool
		err  string
	}{
		{"未诊断", nil, false, ""},
		{"诊断中", &ok, true, ""},
		{"输入错误", nil, false, "请输入域名"},
	} {
		got := uiProbeActions(c.row, c.busy, c.err, 3)
		if len(got) != 1 || got[0].ID != uiIDProbeCopy {
			t.Errorf("%s：只应显示「复制结果」，实际 %v", c.name, ids(got))
		}
		if len(got) == 1 && !got[0].Dim {
			t.Errorf("%s：没有结果可复制时应弱化按钮", c.name)
		}
	}

	got := uiProbeActions(&ok, false, "", 2)
	if len(got) != 1 || got[0].ID != uiIDProbeCopy || got[0].Dim {
		t.Errorf("诊断成功时只应有一个可用的「复制结果」，实际 %v", got)
	}

	got = uiProbeActions(&direct, false, "", 0)
	want := []string{uiIDProbeCopy, uiIDProbeAdd, uiIDProbeMITM, uiIDProbeConfig}
	if strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Errorf("仅直连时应给出 %v，实际 %v", want, ids(got))
	}
	if got[0].Dim {
		t.Error("有诊断结果时「复制结果」应可用")
	}
	for _, a := range got[1:] {
		if a.Dim {
			t.Errorf("失败动作 %q 应可用", a.ID)
		}
		if a.Tip == "" {
			t.Errorf("失败动作 %q 应有悬浮说明", a.ID)
		}
	}

	got = uiProbeActions(&fail, false, "", 2)
	if last := got[len(got)-1]; last.ID != uiIDProbeTodo {
		t.Errorf("有待办时最后应多一个「查看待办」，实际 %v", ids(got))
	}
	got = uiProbeActions(&fail, false, "", 0)
	for _, a := range got {
		if a.ID == uiIDProbeTodo {
			t.Error("没有待办时不该显示「查看待办」")
		}
	}
}

// TestUIProbeText 复制出去的文字必须和界面一致：同一个域名、六行结果、同一句建议。
func TestUIProbeText(t *testing.T) {
	row := selfcheck.Row{
		Host: "github.com", IP: "1.2.3.4", DNSMs: 42, DNSOK: true,
		TCP: "31ms", TCPOK: true, Direct: "HTTP 200", DirectOK: true,
		Proxy: "HTTP 200", ProxyOK: true, ProxyTested: true,
		MITM: "✓", Verdict: selfcheck.VerdictOK,
	}
	got := uiProbeText(&row, false)
	for _, want := range []string{
		"通途诊断：github.com",
		"结论：可用",
		"解析：1.2.3.4 · 42ms",
		"TCP:443：31ms",
		"直连 HTTPS：HTTP 200",
		"经代理：HTTP 200",
		"无SNI反代：✓ 命中（本地解密后剥离 SNI）",
		"建议：",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("复制文本缺少 %q：\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("复制到 Windows 剪贴板应使用 CRLF 换行：%q", got)
	}
	if !strings.Contains(uiProbeText(&row, true), "已暂停") {
		t.Error("暂停时复制的建议也应说明系统流量未经过本地代理")
	}
	if uiProbeText(nil, false) != "" {
		t.Error("没有结果时复制文本应为空（调用方会提示原因）")
	}
}
