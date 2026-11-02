//go:build windows

package main

// 域名诊断面板的窗口侧实现：输入框、打开/关闭、后台诊断、结果绘制。
//
// 纯逻辑（行文案、建议、输入规范化）在 ui_probe.go：那一半不碰窗口，可以单测；
// 这一半只负责把结果放到算好的矩形里，不重复解释结果。

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"tongtu/internal/clipboard"
	"tongtu/internal/selfcheck"
)

// probeTimeout 是窗口一次诊断的整体上限。selfcheck.Prober 自己对单个域名有 20 秒上限，
// 这里再留一点余量：到点后窗口仍能收到一条结果，而不是永远停在「诊断中」。
const probeTimeout = 40 * time.Second

// probeInputText 读取诊断输入框里的内容。
func (w *logWindow) probeInputText() string {
	w.mu.Lock()
	edit := w.probeEdit
	w.mu.Unlock()
	return windowText(edit)
}

// setProbeInputText 写入诊断输入框（只在窗口线程调用）。
func (w *logWindow) setProbeInputText(s string) {
	w.mu.Lock()
	edit := w.probeEdit
	w.mu.Unlock()
	if edit != 0 {
		setWindowText(edit, s)
	}
}

// toggleProbePanel 打开 / 关闭卡片区的域名诊断面板（第二行的「诊断」按钮）。
//
// 与站点面板互斥：打开诊断就离开站点，反之亦然。打开时把焦点给输入框，
// 用户点这个按钮就是想查一个域名，不该再让他点一次输入框。
func (w *logWindow) toggleProbePanel(hwnd windows.Handle) {
	p := w.panelSnapshot()
	open := !p.Open || p.Kind != uiPanelProbe
	w.mu.Lock()
	w.probeOpen = open
	w.sitesOpen = false // 两种面板互斥
	w.mu.Unlock()

	act := currentWindowActions()
	if open && act.Probe == nil {
		// 面板照常打开：把「为什么不能诊断」写在建议区，比只弹一句提示更容易看到。
		w.setProbeFailure("当前实例不支持诊断")
		w.toast("当前实例不支持诊断", styleWarn)
	}

	w.layout()
	if hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}
	if open {
		if edit := w.probeEditHandle(); edit != 0 {
			procSetFocus.Call(uintptr(edit))
		}
		return
	}
	// 关闭面板后输入框被隐藏，焦点得交回窗口，否则键盘会停在一个看不见的控件上。
	if hwnd != 0 {
		procSetFocus.Call(uintptr(hwnd))
	}
}

// runProbeFromInput 读取输入框里的域名并在后台诊断（「诊断」按钮 / 回车）。
//
// 与自检一样放后台：解析、TCP、直连与经代理加起来可能十几秒，窗口线程必须保持响应。
// 诊断期间「诊断」按钮保持弱化，重复触发只提示，不排队。
func (w *logWindow) runProbeFromInput() {
	raw := w.probeInputText()
	if raw == "" {
		w.toast("请先输入域名，例如 github.com", styleWarn)
		return
	}
	host, err := normalizeProbeHost(raw)
	if err != nil {
		w.setProbeFailure(err.Error())
		w.toast(err.Error(), styleWarn)
		return
	}
	act := currentWindowActions()
	if act.Probe == nil {
		w.setProbeFailure("当前实例不支持诊断")
		w.toast("当前实例不支持诊断", styleWarn)
		return
	}

	w.mu.Lock()
	if w.probeBusy {
		w.mu.Unlock()
		w.toast("诊断正在进行，请稍候…", styleMuted)
		return
	}
	w.probeBusy = true
	w.probeTarget = host
	w.probeResult = nil
	w.probeErr = ""
	hwnd := w.hwnd
	w.mu.Unlock()

	// 目标域名进几何缓存键：快照里立刻能看到「正在诊断谁」。
	w.layout()
	if hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		row := act.Probe(ctx, host)

		w.mu.Lock()
		w.probeResult = &row
		w.probeErr = ""
		w.mu.Unlock()
		if hwnd != 0 {
			procPostMessageW.Call(uintptr(hwnd), wmAppProbeDone, 0, 0)
		}
	}()
}

// setProbeFailure 把一次「没跑起来」的诊断写进面板（输入不合法 / 当前实例不支持）。
// 只应由窗口线程调用。
func (w *logWindow) setProbeFailure(msg string) {
	w.mu.Lock()
	w.probeBusy = false
	w.probeResult = nil
	w.probeErr = msg
	w.probeTarget = ""
	hwnd := w.hwnd
	w.mu.Unlock()
	w.layout()
	if hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}
}

// onProbeDone 在窗口线程收尾一次后台诊断：撤掉「诊断中」，重画面板并给一句结论。
func (w *logWindow) onProbeDone() {
	w.mu.Lock()
	w.probeBusy = false
	row := w.probeResult
	w.mu.Unlock()

	if row == nil {
		w.toast("诊断未完成", styleWarn)
	} else {
		style := styleOK
		switch row.Verdict {
		case selfcheck.VerdictDirectOnly:
			style = styleWarn
		case selfcheck.VerdictFail:
			style = styleDanger
		}
		w.toast("诊断完成："+row.Host+" "+row.Verdict.Word(), style)
	}

	w.layout()
	if hwnd := w.handle(); hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}
}

// probeHost 返回这次诊断的目标域名（诊断中、诊断后都有效）。
func (w *logWindow) probeHost() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.probeTarget != "" {
		return w.probeTarget
	}
	if w.probeResult != nil {
		return w.probeResult.Host
	}
	return ""
}

// copyProbeResult 把当前诊断复制成纯文本（「复制结果」）。
//
// 复制的是与屏幕完全一致的内容（uiProbeText 复用行文案与建议），方便用户把它贴给
// 别人排查；诊断本身仍然只读，这里只写剪贴板。
func (w *logWindow) copyProbeResult() {
	w.mu.Lock()
	row, busy, errMsg, paused := w.probeResult, w.probeBusy, w.probeErr, w.status.Paused
	w.mu.Unlock()

	if busy {
		w.toast("诊断正在进行，请稍候…", styleMuted)
		return
	}
	if row == nil {
		if errMsg != "" {
			w.toast("这次诊断没有结果可复制："+errMsg, styleWarn)
		} else {
			w.toast("还没有诊断结果，先输入域名点「诊断」", styleMuted)
		}
		return
	}
	if err := clipboard.Copy(uiProbeText(row, paused)); err != nil {
		w.report(uiIDProbeCopy, err)
		return
	}
	w.toast("诊断结果已复制到剪贴板", styleOK)
}

// probeAddSite 把诊断目标加入自定义站点（失败时的「加入站点」）。
//
// 先在后台读一次站点列表：已经在列表里就只提示，不派发一次注定失败的写入。
// 写配置与重建规则放在 goroutine 上，窗口线程只负责反馈，避免界面卡住。
func (w *logWindow) probeAddSite() {
	act := currentWindowActions()
	if act.AddSite == nil {
		w.toast("当前实例无法添加自定义站点", styleWarn)
		return
	}
	host := w.probeHost()
	if host == "" {
		w.toast("没有可加入的域名", styleWarn)
		return
	}
	if !w.siteBusyBegin() {
		w.toast("正在处理，请稍候…", styleMuted)
		return
	}
	hwnd := w.handle()
	w.toast("正在加入站点 "+host+"…", styleMuted)
	go func() {
		defer w.siteBusyEnd()
		if act.Sites != nil {
			for _, s := range act.Sites() {
				if strings.EqualFold(s.Pattern, host) {
					w.toast(host+" 已经在自定义站点里", styleMuted)
					return
				}
			}
		}
		if _, err := act.AddSite(host); err != nil {
			w.report(uiIDProbeAdd, err)
			return
		}
		w.toast("已加入站点 "+host+"，立即生效", styleOK)
		w.applySites(hwnd, act, false)
		if act.Refresh != nil {
			act.Refresh()
		}
	}()
}

// probeOpenMITM 为诊断目标打开无 SNI 反代（失败时的「打开反代」）。
//
// 与开关反代走同一套前置检查：缺根证书先问一次并支持一键安装；目标还不在站点里
// 就先加进去。成功后不自动重跑诊断——用户想复查可以再点「诊断」，那样更容易看懂
// 「刚才那次结果」和「现在又测了一次」的区别。
func (w *logWindow) probeOpenMITM() {
	act := currentWindowActions()
	if act.AddSite == nil || act.SetSiteMITM == nil {
		w.toast("当前实例无法修改反代设置", styleWarn)
		return
	}
	host := w.probeHost()
	if host == "" {
		w.toast("没有可处理的域名", styleWarn)
		return
	}
	if !w.siteBusyBegin() {
		w.toast("正在处理，请稍候…", styleMuted)
		return
	}
	hwnd := w.handle()
	w.toast("正在为 "+host+" 打开反代…", styleMuted)
	go func() {
		defer w.siteBusyEnd()
		if act.CertReady != nil && !act.CertReady() {
			if act.InstallCert == nil {
				w.report(uiIDProbeMITM, fmt.Errorf("根证书未安装，且当前实例不支持一键安装"))
				return
			}
			if !confirmInstallCert() {
				w.toast("已取消：开启无 SNI 反代需要根证书", styleMuted)
				return
			}
			w.toast("正在安装根证书（可能弹出系统确认框，请留意屏幕）…", styleMuted)
			if err := act.InstallCert(); err != nil {
				w.report(uiIDProbeMITM, err)
				return
			}
		}

		inSites := false
		if act.Sites != nil {
			for _, s := range act.Sites() {
				if strings.EqualFold(s.Pattern, host) {
					inSites = true
					if s.MITM {
						w.toast(host+" 的反代已经开着", styleMuted)
						return
					}
					break
				}
			}
		}
		if !inSites {
			if _, err := act.AddSite(host); err != nil {
				w.report(uiIDProbeMITM, err)
				return
			}
		}
		if err := act.SetSiteMITM(host, true); err != nil {
			w.report(uiIDProbeMITM, err)
			return
		}
		w.toast("已为 "+host+" 打开反代；再点「诊断」可复查", styleOK)
		w.applySites(hwnd, act, false)
		if act.Refresh != nil {
			act.Refresh()
		}
	}()
}

// paintProbePanel 画诊断面板：固定 6 行（标签 + 值）、下一步建议与底部说明。
//
// 行内没有任何自绘按钮（诊断按钮在工具条里，由 paintHeader 统一画），
// 因此这里只处理文字与颜色；几何来自 uiLayout，与命中共用同一份。
func (w *logWindow) paintProbePanel(hdc uintptr, l uiLayout) {
	w.mu.Lock()
	theme, font, dpi := w.theme, w.font, w.dpi
	row, busy, errMsg := w.probeResult, w.probeBusy, w.probeErr
	paused := w.status.Paused
	w.mu.Unlock()
	if hdc == 0 || font == 0 {
		return
	}
	procSetBkMode.Call(hdc, bkTransparent)
	procSelectObject.Call(hdc, font)

	lines := uiProbeLines(row, busy, errMsg)
	for i, r := range l.ProbeRows {
		if i >= len(lines) {
			break
		}
		cy := (r.Top + r.Bottom) / 2
		procSetTextColor.Call(hdc, uintptr(theme.muted))
		drawTextLine(hdc, lines[i].Label, r.Left, cy, l.ProbeLabelW, dtLeft)
		procSetTextColor.Call(hdc, uintptr(probeToneColor(theme, lines[i].Tone)))
		drawTextLine(hdc, lines[i].Value, r.Left+l.ProbeLabelW, cy, r.Right-r.Left-l.ProbeLabelW, dtLeft)
	}

	// 建议区：失败时用失败色（它同时说明了「下一步做什么」），其余保持二级文字。
	advice := uiProbeAdvice(row, busy, errMsg, paused)
	color := theme.muted
	if errMsg != "" {
		color = theme.danger
	}
	procSetTextColor.Call(hdc, uintptr(color))
	drawTextParagraph(hdc, advice, l.ProbeAdvice)

	procSetTextColor.Call(hdc, uintptr(theme.faint))
	drawTextInRect(hdc, uiProbeHint, l.ProbeHint, dtLeft)

	// 建议与底部说明之间画一条发丝线（与站点面板的层次一致）。
	if y := l.ProbeHint.Top - scaleDPI(3, dpi); y > l.ProbeAdvice.Top && theme.hairlineBrush != 0 {
		line := rectT{Left: l.ProbeHint.Left, Top: y, Right: l.ProbeHint.Right, Bottom: y + 1}
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&line)), theme.hairlineBrush)
	}
}

// drawTextParagraph 在一个矩形里画一段左对齐、可换行的说明（从顶部开始）。
// 与 drawTextBlock 的区别：空状态提示适合居中，而「下一步做什么」是一段话，
// 左对齐更好读。
func drawTextParagraph(hdc uintptr, text string, r rectT) {
	if hdc == 0 || text == "" || r.Right <= r.Left || r.Bottom <= r.Top {
		return
	}
	p := utf16Ptr(text)
	if p == nil {
		return
	}
	rc := r
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), ^uintptr(0),
		uintptr(unsafe.Pointer(&rc)),
		dtLeft|dtWordBreak|dtNoPrefix)
}

// probeToneColor 把行语气映射到主题色：与状态条 / 日志共用同一套语义色，
// 不为诊断面板引入新颜色。
func probeToneColor(t uiTheme, tone uiProbeTone) uint32 {
	switch tone {
	case probeToneOK:
		return t.ok
	case probeToneWarn:
		return t.warn
	case probeToneErr:
		return t.danger
	case probeToneMuted:
		return t.faint
	default:
		return t.text
	}
}
