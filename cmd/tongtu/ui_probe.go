package main

// 「诊断」面板的纯逻辑：把 selfcheck.Row 变成界面上的几行文字，并给出下一步建议。
//
// 与绘制分开：这里不碰窗口句柄，因此可以单测每一种结论（可用 / 仅直连 / 不通 /
// 未诊断 / 诊断中），绘制层只负责把标签、值与语气画到对应的矩形里。

import (
	"fmt"
	"strings"

	"tongtu/internal/config"
	"tongtu/internal/selfcheck"
)

// uiProbeTone 决定一行值的颜色：文字色 / 成功 / 警告 / 失败 / 弱化。
// 颜色语义与状态条、日志一致（绿=好、橙=需注意、红=失败），不引入新含义。
type uiProbeTone int

const (
	probeToneText uiProbeTone = iota
	probeToneOK
	probeToneWarn
	probeToneErr
	probeToneMuted
)

// uiProbeLine 是诊断面板里的一行：左侧固定标签 + 右侧值。
type uiProbeLine struct {
	Label string
	Value string
	Tone  uiProbeTone
}

// uiProbeLabels 是诊断面板的固定行序（几何层据此算固定行数，内容随状态变化）。
var uiProbeLabels = []string{"结论", "解析", "TCP:443", "直连 HTTPS", "经代理", "无SNI反代"}

const (
	uiProbeTip = "诊断：检查单个域名的干净解析、直连、经代理与反代命中，失败时给出下一步；再点一次回到日志"
	// uiProbeHint 是面板底部的一行说明：把「只读」与「不修改配置」说清楚。
	uiProbeHint = "只检查这一个域名，不修改任何配置；结果按当前运行状态计算。"
	// uiProbeInputCue 是输入框的占位提示。
	uiProbeInputCue = "输入域名，例如 github.com"
	// uiProbeRunTip 是「诊断」按钮的悬浮说明。
	uiProbeRunTip = "诊断：测试输入框里的域名（回车也可以）；诊断期间可以继续使用窗口"
)

// uiProbeLines 把一次诊断结果摊成固定 6 行。
//
// 固定行数让几何不随结果跳动：诊断前、诊断中、诊断后，输入框与各行位置都不变，
// 用户不会看到面板「长高」或「缩短」。没有数据的格子写「—」，不是留空。
func uiProbeLines(row *selfcheck.Row, busy bool, errMsg string) []uiProbeLine {
	blank := func(label, value string, tone uiProbeTone) uiProbeLine {
		return uiProbeLine{Label: label, Value: value, Tone: tone}
	}
	if busy {
		out := []uiProbeLine{blank(uiProbeLabels[0], "诊断中…", probeToneMuted)}
		for _, label := range uiProbeLabels[1:] {
			out = append(out, blank(label, "—", probeToneMuted))
		}
		return out
	}
	if errMsg != "" {
		out := []uiProbeLine{blank(uiProbeLabels[0], "诊断失败", probeToneErr)}
		for _, label := range uiProbeLabels[1:] {
			out = append(out, blank(label, "—", probeToneMuted))
		}
		return out
	}
	if row == nil {
		out := make([]uiProbeLine, 0, len(uiProbeLabels))
		out = append(out, blank(uiProbeLabels[0], "未诊断", probeToneMuted))
		for _, label := range uiProbeLabels[1:] {
			out = append(out, blank(label, "—", probeToneMuted))
		}
		return out
	}

	verdictTone := probeToneOK
	switch row.Verdict {
	case selfcheck.VerdictDirectOnly:
		verdictTone = probeToneWarn
	case selfcheck.VerdictFail:
		verdictTone = probeToneErr
	}

	ip := "失败"
	ipTone := probeToneErr
	if row.DNSOK {
		ip = fmt.Sprintf("%s · %dms", row.IP, row.DNSMs)
		ipTone = probeToneText
	}
	tcp := "—"
	tcpTone := probeToneMuted
	if row.DNSOK {
		tcp, tcpTone = row.TCP, probeToneText
		if !row.TCPOK {
			tcpTone = probeToneErr
		}
	}
	direct := row.Direct
	if direct == "" {
		direct = "—"
	}
	proxy, proxyTone := row.Proxy, probeToneMuted
	if proxy == "" {
		proxy = "未测（本地代理未运行）"
	}
	if row.ProxyTested {
		proxy = row.Proxy
		proxyTone = probeToneWarn
		if row.ProxyOK {
			proxyTone = probeToneOK
		}
	}
	mitm := "—"
	switch row.MITM {
	case "✓":
		mitm = "✓ 命中（本地解密后剥离 SNI）"
	case "–":
		mitm = "– 不经过（保持隧道）"
	case "未启用":
		mitm = "未启用"
	}

	return []uiProbeLine{
		blank(uiProbeLabels[0], row.Verdict.Word(), verdictTone),
		blank(uiProbeLabels[1], ip, ipTone),
		blank(uiProbeLabels[2], tcp, tcpTone),
		blank(uiProbeLabels[3], direct, probeToneText),
		blank(uiProbeLabels[4], proxy, proxyTone),
		blank(uiProbeLabels[5], mitm, probeToneMuted),
	}
}

// uiProbeAdvice 给出「下一步做什么」：诊断的价值在于告诉用户卡在哪，而不是只列数字。
//
// paused 为真时，经代理测试只说明本地代理可用，系统流量并没有经过它——必须说清楚，
// 否则用户会以为「诊断通过 = 已经在加速」。
func uiProbeAdvice(row *selfcheck.Row, busy bool, errMsg string, paused bool) string {
	if busy {
		return "正在解析、直连与经代理测试，通常 1–10 秒；结果出来前可以继续用窗口。"
	}
	if errMsg != "" {
		return errMsg
	}
	if row == nil {
		return "输入域名后点「诊断」（回车也可以）。只检查这一个域名，不会修改任何配置。"
	}
	// 暂停时「经代理通」最容易被误读成「已经在加速」：这种结果必须整句改写。
	// 其余结论（解析失败 / TCP 不通 / 仅直连）本身就是用户更需要的下一步，
	// 保留原建议，只在句尾补一句「当前已暂停」，不把有用信息顶掉。
	if paused && row.ProxyOK {
		return "当前已暂停加速：经代理结果只说明本地代理可用，系统流量并未经过它。"
	}
	var advice string
	switch row.Verdict {
	case selfcheck.VerdictOK:
		if !row.ProxyTested {
			advice = "本地代理未运行，只验证了直连；启动通途后再诊断可确认加速效果。"
			break
		}
		if row.MITM == "✓" {
			advice = "加速已生效：该域名命中无 SNI 反代，经本地代理返回 " + row.Proxy + "。"
			break
		}
		advice = "加速已生效：经本地代理返回 " + row.Proxy + "。"
	case selfcheck.VerdictDirectOnly:
		advice = "能直连但经代理不通：检查通途是否在运行、端口是否被占用，并查看日志；" +
			"若该站确实被 SNI 封锁，可在「站点」里为它打开反代。"
	default:
		if !row.DNSOK {
			advice = "解析失败：检查网络与 DNS 端点（dns.endpoints）；本地代理未运行时也能诊断解析。"
		} else if !row.TCPOK {
			advice = "干净解析出了 IP，但 TCP:443 不可达：换一个优选 IP（dns.static_hosts）或稍后重试。"
		} else if row.MITM == "✓" {
			advice = "两条路都不通：该域名已配置反代，检查根证书是否已安装、状态条里的反代是否真的启用。"
		} else {
			advice = "两条路都不通：若该站确实被 SNI 封锁，可在「站点」里把它加入并打开反代；" +
				"否则先查看日志里的失败原因。"
		}
	}
	if paused {
		advice += "（当前已暂停，系统流量并未经过本地代理。）"
	}
	return advice
}

// normalizeProbeHost 规范化诊断输入：允许直接粘贴浏览器地址，但拒绝通配写法
// （诊断需要一个具体域名；通配是规则，不是可连接的目标）。
func normalizeProbeHost(raw string) (string, error) {
	p, err := config.NormalizePattern(raw)
	if err != nil {
		return "", err
	}
	if strings.Contains(p, "*") {
		return "", fmt.Errorf("诊断需要具体域名，不能用通配写法：%s", p)
	}
	return p, nil
}
