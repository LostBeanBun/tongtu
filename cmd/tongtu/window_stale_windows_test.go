//go:build windows

package main

// 日志控件的「残影」回归测试。
//
// 背景：追加日志与滚动如果发生在 WM_SETREDRAW(FALSE) 期间，控件会用 ScrollWindow 直接搬动
// 屏幕像素却不重画被露出的那一条，旧文字就会与新文字叠在一起（日志刷得快时肉眼可见）。
// 这类问题在 WM_PRINTCLIENT 渲染里看不出来（它每次都重新画），因此这里比较的是
// 「控件自己 DC 上真实存在的像素」（用户屏幕上看到的）与「重新画一遍的像素」。
//
// 判据只看墨量：撕裂会在某一行的墨量上明显多出一份，而抗锯齿差异只会让墨量小幅波动。

import (
	"fmt"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// livePixels 取控件自己 DC 上的像素（= 屏幕上的内容）。
func livePixels(edit windows.Handle, w, h int32) *memDC {
	m := newMemDC(w, h)
	if m == nil {
		return nil
	}
	hdc, _, _ := procGetDC.Call(uintptr(edit))
	if hdc == 0 {
		m.close()
		return nil
	}
	procBitBlt.Call(m.dc, 0, 0, uintptr(w), uintptr(h), hdc, 0, 0, srcCopy)
	procReleaseDC.Call(uintptr(edit), hdc)
	return m
}

// refPixels 请控件重新画一遍到离屏 DC（同一块区域、同一尺寸）。
func refPixels(edit windows.Handle, w, h int32) *memDC {
	m := newMemDC(w, h)
	if m == nil {
		return nil
	}
	procSendMessageW.Call(uintptr(edit), wmPrintClient, m.dc, 0)
	return m
}

// inkRows 统计每行的墨量：与该帧背景亮度差超过阈值的像素数。
// 背景取该帧出现最多的亮度值（日志区绝大部分像素都是底色）。
func inkRows(m *memDC, w, h int32) []int {
	lum := func(c uint32) int {
		r := int((c >> 16) & 0xFF)
		g := int((c >> 8) & 0xFF)
		b := int(c & 0xFF)
		return (299*r + 587*g + 114*b) / 1000
	}
	hist := map[int]int{}
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			hist[lum(m.pixel(x, y))]++
		}
	}
	bg, best := 0, -1
	for l, n := range hist {
		if n > best {
			bg, best = l, n
		}
	}
	rows := make([]int, h)
	for y := int32(0); y < h; y++ {
		n := 0
		for x := int32(0); x < w; x++ {
			d := lum(m.pixel(x, y)) - bg
			if d < 0 {
				d = -d
			}
			if d > 40 {
				n++
			}
		}
		rows[y] = n
	}
	return rows
}

// TestLogAppendLeavesNoStaleInk 追加日志后，控件 DC 上不该出现「比重新画一遍还多的墨」。
func TestLogAppendLeavesNoStaleInk(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setStatusForInteraction(false, false, false)
	lw.clearLog()
	t.Cleanup(func() { lw.clearLog() }) // 别把长日志留给其它测试（滚动条会改变客户区宽度）
	edit := lw.editHandle()
	if edit == 0 {
		t.Fatal("日志控件不存在")
	}
	var cr rectT
	procGetClientRect.Call(uintptr(edit), uintptr(unsafe.Pointer(&cr)))
	w, h := cr.Right, cr.Bottom
	if w < 200 || h < 100 {
		t.Fatalf("日志控件客户区太小：%dx%d", w, h)
	}

	// 先把日志填到要滚动，再逐条追加（每条记录都带续行，和真实日志一样）
	for i := 0; i < 30; i++ {
		emitWarnLog(t, fmt.Sprintf("第 %d 条记录（把日志顶到需要滚动）", i))
	}

	for i := 0; i < 8; i++ {
		emitWarnLog(t, fmt.Sprintf("追加记录 %d", i))

		live, ref := livePixels(edit, w, h), refPixels(edit, w, h)
		if live == nil || ref == nil {
			t.Fatal("取不到像素")
		}
		lr, rr := inkRows(live, w, h), inkRows(ref, w, h)
		live.close()
		ref.close()

		totalLive, totalRef := 0, 0
		for y := range lr {
			totalLive += lr[y]
			totalRef += rr[y]
		}
		if totalLive == 0 {
			t.Fatalf("日志控件 DC 上一片空白（测试前提不成立）")
		}
		for y := range lr {
			// 撕裂会让某一行的墨量差不多翻倍；抗锯齿差异只在个位数像素上抖动。
			if rr[y] > 20 && lr[y] > 2*rr[y]+20 {
				t.Errorf("第 %d 次追加后 y=%d 行的墨量异常：DC 上 %d，重画只有 %d（疑似残影/文字重叠）",
					i, y, lr[y], rr[y])
			}
		}
		t.Logf("第 %d 次追加：DC 墨量=%d 重画墨量=%d", i, totalLive, totalRef)
	}
}
